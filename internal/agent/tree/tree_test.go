package tree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	_ "github.com/edotau/claude-code/internal/claude" // the vendors register their roster rows from their own init
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// pinNow returns Options.Now pinned to t, so running/finished/stale classification never depends on
// wall-clock time.
func pinNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// copyTestdataTree copies testdata/projects into a fresh temp claudeDir and backdates every session
// transcript's mtime to match its own content timestamp (or leaves running-session's mtime at "now"),
// so status classification below the glob layer is driven by mtime, not file-write-order.
func copyTestdataTree(t *testing.T) (claudeDir string, now time.Time) {
	t.Helper()
	now = time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	claudeDir = t.TempDir()
	copyTestdataFiles(t, filepath.Join("testdata", "projects"), filepath.Join(claudeDir, "projects"))
	backdateFixtures(t, filepath.Join(claudeDir, "projects", "-Users-fake-demo-repo"), now)
	return claudeDir, now
}

func copyTestdataFiles(t *testing.T, src, dstRoot string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(dstRoot, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, raw, 0o600)
	})
	if err != nil {
		t.Fatalf("copy testdata: %v", err)
	}
}

// backdateFixtures sets each fixture's age relative to now: finished-session and its subagents/workflow stay
// just inside the active window so Build's --days scoping finds them; running-session is fresh; stale is far out.
func backdateFixtures(t *testing.T, proj string, now time.Time) {
	t.Helper()
	mtimes := map[string]time.Time{
		"finished-session-aaaa1111.jsonl":  now.Add(-5 * time.Minute),
		"running-session-bbbb2222.jsonl":   now,
		"stale-session-cccc3333.jsonl":     now.Add(-30 * 24 * time.Hour),
		"malformed-session-dddd4444.jsonl": now.Add(-time.Minute),
	}
	for name, mt := range mtimes {
		p := filepath.Join(proj, name)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
	// The finished session's subagents/workflow files: agent-fakeagentid001 (finished) can be old,
	// agent-fakeagentid002 (unfinished) must sit inside activeWindow to classify "running".
	chtimesAll(t, filepath.Join(proj, "finished-session-aaaa1111"), now.Add(-5*time.Minute))
	if err := os.Chtimes(
		filepath.Join(proj, "finished-session-aaaa1111", "subagents", "agent-fakeagentid002.jsonl"),
		now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// workflowByID finds one run in a session's Workflows slice, failing the test when absent.
func workflowByID(t *testing.T, runs []WorkflowRun, id string) WorkflowRun {
	t.Helper()
	for _, r := range runs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("workflow run %q not found among %d runs", id, len(runs))
	return WorkflowRun{}
}

// chtimesAll backdates every regular file under dir to mt.
func chtimesAll(t *testing.T, dir string, mt time.Time) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		return os.Chtimes(path, mt, mt)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// sessionByID finds one session in the built tree, failing the test when absent.
func sessionByID(t *testing.T, tree *Tree, id string) Session {
	t.Helper()
	for _, s := range tree.Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %q not found among %d sessions", id, len(tree.Sessions))
	return Session{}
}

// (a) Sessions and their subagents are discovered, and a subagent's spawnDepth rides straight through
// from its sidecar meta.json into Subagent.Depth.
func TestBuildDiscoversSessionsAndSubagentsByDepth(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	if len(s.Subagents) != 2 {
		t.Fatalf("got %d subagents, want 2: %+v", len(s.Subagents), s.Subagents)
	}
	byID := map[string]Subagent{}
	for _, sub := range s.Subagents {
		byID[sub.ID] = sub
	}
	if got := byID["fakeagentid001"].Depth; got != 1 {
		t.Errorf("agent001 depth = %d, want 1", got)
	}
	if got := byID["fakeagentid002"].Depth; got != 2 {
		t.Errorf("agent002 depth = %d, want 2", got)
	}
	if got := byID["fakeagentid001"].Type; got != "copilot" {
		t.Errorf("agent001 type = %q, want copilot", got)
	}
}

// (a continued) The session's one workflow run folds its two agent transcripts into their declared
// phases (Detect, Gate), each phase holding the agents that ran inside it.
func TestBuildFoldsWorkflowRunIntoPhases(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	// 5 runs in this fixture: wf_fake0001-abc (manifest), wf_fake0002-nomanifest (manifest-less,
	// finished via journal), wf_fake0003-running (manifest-less, still running via journal),
	// wf_fake0004-error (manifest-less, one agent errored via a null journal result),
	// wf_fake0005-killed (manifest with a null result but a top-level status:"killed").
	if len(s.Workflows) != 5 {
		t.Fatalf("got %d workflow runs, want 5: %+v", len(s.Workflows), s.Workflows)
	}
	run := workflowByID(t, s.Workflows, "wf_fake0001-abc")
	if run.Name != "gate-loop" || run.Status != "green" {
		t.Errorf("run = %+v, want name=gate-loop status=green", run)
	}
	killed := workflowByID(t, s.Workflows, "wf_fake0005-killed")
	if killed.Name != "killed-run" || killed.Status != StatusError {
		t.Errorf("killed run = %+v, want name=killed-run status=%s", killed, StatusError)
	}
	assertPhaseAgents(t, run, map[string]string{"Detect": "fakewfagent001", "Gate": "fakewfagent002"})
}

// assertPhaseAgents requires each named phase to hold exactly the one wanted agent.
func assertPhaseAgents(t *testing.T, run WorkflowRun, want map[string]string) {
	t.Helper()
	phases := map[string][]string{}
	for _, p := range run.Phases {
		for _, a := range p.Agents {
			phases[p.Name] = append(phases[p.Name], a.ID)
		}
	}
	for name, id := range want {
		if got := phases[name]; len(got) != 1 || got[0] != id {
			t.Errorf("%s phase agents = %v, want [%s]", name, got, id)
		}
	}
}

// (b) Status classification: a session/subagent written within activeWindow of the pinned clock is
// active/running; finished wins outright for a subagent regardless of mtime; otherwise idle/stale.
func TestStatusClassificationUsesPinnedClock(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	running := sessionByID(t, tree, "running-session-bbbb2222")
	if running.Status != statusActive {
		t.Errorf("running-session status = %q, want active", running.Status)
	}
	idle := sessionByID(t, tree, "stale-session-cccc3333")
	if idle.Status != statusIdle {
		t.Errorf("stale-session status = %q, want idle", idle.Status)
	}
	finishedSession := sessionByID(t, tree, "finished-session-aaaa1111")
	byID := map[string]Subagent{}
	for _, sub := range finishedSession.Subagents {
		byID[sub.ID] = sub
	}
	if got := byID["fakeagentid001"].Status; got != StatusFinished {
		t.Errorf("agent001 (finished:true) status = %q, want finished", got)
	}
	if got := byID["fakeagentid002"].Status; got != StatusRunning {
		t.Errorf("agent002 (finished:false, fresh mtime) status = %q, want running", got)
	}
}

// (b continued) An unfinished subagent whose transcript mtime falls outside activeWindow is stale.
func TestSubagentGoesStaleOutsideActiveWindow(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	// Push agent002 well past activeWindow without touching "finished" (still false in its meta).
	p := filepath.Join(claudeDir, "projects", "-Users-fake-demo-repo", "finished-session-aaaa1111",
		"subagents", "agent-fakeagentid002.jsonl")
	old := now.Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	for _, sub := range s.Subagents {
		if sub.ID == "fakeagentid002" && sub.Status != StatusStale {
			t.Errorf("agent002 status = %q, want stale", sub.Status)
		}
	}
}

// (c) --days excludes a session whose transcript mtime falls outside the window.
func TestBuildDaysFilterExcludesOldSessions(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 1, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range tree.Sessions {
		if s.ID == "stale-session-cccc3333" {
			t.Fatalf("stale-session-cccc3333 (30 days old) should be excluded by --days 1")
		}
	}
	found := false
	for _, s := range tree.Sessions {
		if s.ID == "finished-session-aaaa1111" {
			found = true
		}
	}
	if !found {
		t.Fatal("finished-session-aaaa1111 (5 minutes old) should survive --days 1")
	}
}

// (d) --session narrows to exactly one session id, searched across every project dir, ignoring --days.
func TestBuildSessionFilter(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Session: "stale-session-cccc3333", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Sessions) != 1 || tree.Sessions[0].ID != "stale-session-cccc3333" {
		t.Fatalf("got %d sessions, want exactly [stale-session-cccc3333]: %+v", len(tree.Sessions), tree.Sessions)
	}
}

// (e) --running keeps only a session whose own status is active or that has a running subagent
// (direct or inside a workflow run); a wholly finished/idle session is dropped.
func TestBuildRunningFilter(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range tree.Sessions {
		ids[s.ID] = true
	}
	if !ids["running-session-bbbb2222"] {
		t.Error("running-session-bbbb2222 (active) should survive --running")
	}
	if !ids["finished-session-aaaa1111"] {
		t.Error("finished-session-aaaa1111 (has a running subagent) should survive --running")
	}
	if ids["stale-session-cccc3333"] {
		t.Error("stale-session-cccc3333 (idle, no running subagent) should be dropped by --running")
	}
}

type schemaPhase struct {
	Name   string `json:"name"`
	Agents []struct {
		ID string `json:"id"`
	} `json:"agents"`
}

type schemaSession struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Cwd     string `json:"cwd"`
	Branch  string `json:"branch"`
	Title   string `json:"title"`
	Updated string `json:"updated"`
	Status  string `json:"status"`
	Tokens  struct {
		In  int64 `json:"in"`
		Out int64 `json:"out"`
	} `json:"tokens"`
	Subagents []struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Depth       int    `json:"depth"`
		LastTool    string `json:"lastTool"`
	} `json:"subagents"`
	Workflows []struct {
		ID     string        `json:"id"`
		Name   string        `json:"name"`
		Status string        `json:"status"`
		Phases []schemaPhase `json:"phases"`
	} `json:"workflows"`
}

// (f) --format json's shape round-trips through the documented schema: {sessions:[{id, project, cwd,
// branch, title, updated, status, tokens, subagents:[...], workflows:[...]}]}.
func TestBuildJSONSchema(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Session: "finished-session-aaaa1111", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Sessions []schemaSession `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal into documented schema: %v\nraw: %s", err, raw)
	}
	if len(decoded.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(decoded.Sessions))
	}
	assertSchemaSession(t, decoded.Sessions[0])
}

func assertSchemaSession(t *testing.T, s schemaSession) {
	t.Helper()
	if s.ID != "finished-session-aaaa1111" || s.Project != "-Users-fake-demo-repo" || s.Cwd != "/Users/fake/demo-repo" ||
		s.Branch != "main" || s.Status != "idle" {
		t.Errorf("session fields = %+v", s)
	}
	if len(s.Subagents) != 2 {
		t.Errorf("got %d subagents in JSON, want 2", len(s.Subagents))
	}
	if len(s.Workflows) != 5 {
		t.Fatalf("got %d workflows, want 5: %+v", len(s.Workflows), s.Workflows)
	}
	assertMainRunPhases(t, s)
}

func assertMainRunPhases(t *testing.T, s schemaSession) {
	t.Helper()
	var mainPhases []schemaPhase
	for _, r := range s.Workflows {
		if r.ID == "wf_fake0001-abc" {
			mainPhases = r.Phases
		}
	}
	if len(mainPhases) != 2 {
		t.Errorf("wf_fake0001-abc phases = %+v, want 2", mainPhases)
	}
}

// (h) A session whose last transcript line is truncated/malformed still renders without panicking;
// the earlier valid lines still populate cwd/branch/title.
func TestMalformedLastLineDoesNotPanic(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("build panicked on a malformed last line: %v", r)
		}
	}()
	tree, err := build(claudeDir, Options{Session: "malformed-session-dddd4444", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "malformed-session-dddd4444")
	if s.Cwd != "/Users/fake/demo-repo" || s.Branch != "main" {
		t.Errorf("session from earlier valid lines = %+v", s)
	}
}

// Tokens.CacheRead rides the JSON wire separately from In (input+cache-creation), per the doc comment
// on the Tokens type; a cache-read-heavy agent must not be folded into the "freshly billed" column.
func TestTokensCacheReadIsSeparateFromIn(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Session: "finished-session-aaaa1111", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	var agent001 Subagent
	for _, sub := range s.Subagents {
		if sub.ID == "fakeagentid001" {
			agent001 = sub
		}
	}
	// Fixture usage: input_tokens=2, cache_creation_input_tokens=27907, cache_read_input_tokens=0.
	if agent001.Tokens.In != 2+27907 {
		t.Errorf("agent001 Tokens.In = %d, want %d (input+cache-creation)", agent001.Tokens.In, 2+27907)
	}
	if agent001.Tokens.CacheRead != 0 {
		t.Errorf("agent001 Tokens.CacheRead = %d, want 0", agent001.Tokens.CacheRead)
	}
	raw, err := json.Marshal(s.Tokens)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cacheRead"`) {
		t.Errorf("Tokens JSON missing cacheRead key: %s", raw)
	}
}

// filterIdleSessions drops a session with no subagents/workflows whose status is idle, and keeps
// everything else (active sessions, or any session that dispatched something).
func TestFilterIdleSessionsDropsOnlyEmptyIdle(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	kept := filterIdleSessions(tree.Sessions)
	ids := map[string]bool{}
	for _, s := range kept {
		ids[s.ID] = true
	}
	if ids["stale-session-cccc3333"] {
		t.Error("stale-session-cccc3333 (idle, no subagents/workflows) should be dropped")
	}
	for _, want := range []string{"running-session-bbbb2222", "finished-session-aaaa1111", "malformed-session-dddd4444"} {
		if !ids[want] {
			t.Errorf("%s should survive filterIdleSessions (active, or has subagents/workflows)", want)
		}
	}
	if len(kept) != len(tree.Sessions)-1 {
		t.Errorf("got %d kept sessions, want %d (one idle-empty dropped)", len(kept), len(tree.Sessions)-1)
	}
}

// A session with no subagents/workflows that is still active (fresh mtime) is NOT idle, so
// filterIdleSessions must keep it regardless of its empty subagent/workflow list.
func TestFilterIdleSessionsKeepsActiveEvenWhenEmpty(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Session: "running-session-bbbb2222", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "running-session-bbbb2222")
	if len(s.Subagents) != 0 || len(s.Workflows) != 0 {
		t.Fatalf("precondition: running-session-bbbb2222 fixture must have no subagents/workflows, got %+v", s)
	}
	kept := filterIdleSessions(tree.Sessions)
	if len(kept) != 1 {
		t.Fatalf("got %d sessions after filterIdleSessions, want 1 (active session kept)", len(kept))
	}
}

// --session must be a plain id: no path separator and no ".." component, since it feeds a
// filepath.Join/Glob pattern directly.
func TestBuildRejectsSessionPathTraversal(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	for _, bad := range []string{"../../etc/passwd", "foo/../bar", ".."} {
		if _, err := build(claudeDir, Options{Session: bad, Now: pinNow(now)}); err == nil {
			t.Errorf("build(Session=%q) = nil error, want rejection of a non-plain session id", bad)
		}
	}
}

// runningIDs collects every subagent id in a session, direct and inside workflow phases.
func runningIDs(s Session) (subs, wfAgents []string, runs []string) {
	for _, sub := range s.Subagents {
		subs = append(subs, sub.ID)
	}
	for _, r := range s.Workflows {
		runs = append(runs, r.ID)
		for _, p := range r.Phases {
			for _, a := range p.Agents {
				wfAgents = append(wfAgents, a.ID)
			}
		}
	}
	return
}

// --running prunes finished/error subagents and workflow runs inside a kept session; running ones stay.
func TestBuildRunningPrunesFinishedChildren(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	subs, _, runs := runningIDs(s)
	if len(subs) != 1 || subs[0] != "fakeagentid002" {
		t.Errorf("subagents = %v, want only the running [fakeagentid002]", subs)
	}
	for _, id := range runs {
		if id == "wf_fake0001-abc" || id == "wf_fake0005-killed" {
			t.Errorf("--running kept wholly finished/killed run %s (runs=%v)", id, runs)
		}
	}
	assertNoDoneWorkflowAgents(t, s)
	if !slices.Contains(runs, "wf_fake0003-running") {
		t.Errorf("running workflow run wf_fake0003-running dropped (runs=%v)", runs)
	}
}

func assertNoDoneWorkflowAgents(t *testing.T, s Session) {
	t.Helper()
	for _, r := range s.Workflows {
		for _, p := range r.Phases {
			for _, a := range p.Agents {
				if a.Status == StatusFinished || a.Status == StatusError {
					t.Errorf("--running kept %s workflow agent %s", a.Status, a.ID)
				}
			}
		}
	}
}

// Plain build (no --running) keeps both the finished and the running subagent.
func TestBuildPlainKeepsFinishedChildren(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	subs, _, runs := runningIDs(sessionByID(t, tree, "finished-session-aaaa1111"))
	if len(subs) != 2 {
		t.Errorf("plain subagents = %v, want both fakeagentid001 and 002", subs)
	}
	if len(runs) != 5 {
		t.Errorf("plain runs = %v, want all 5", runs)
	}
}

// A stale (unfinished, past the window) subagent is kept by --running: it may be hung, not done.
func TestBuildRunningKeepsStaleSubagent(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	p := filepath.Join(claudeDir, "projects", "-Users-fake-demo-repo", "finished-session-aaaa1111",
		"subagents", "agent-fakeagentid002.jsonl")
	old := now.Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	tree, err := build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	if len(s.Subagents) != 1 || s.Subagents[0].ID != "fakeagentid002" || s.Subagents[0].Status != StatusStale {
		t.Errorf("subagents = %+v, want the stale fakeagentid002 kept", s.Subagents)
	}
}

// The JSON view marshals the same pruned tree: no finished/error subagents, same session count.
func TestBuildRunningJSONMatchesPrunedTree(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Sessions []struct {
			ID        string `json:"id"`
			Subagents []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"subagents"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sessions) != len(tree.Sessions) {
		t.Fatalf("json sessions = %d, tree = %d", len(decoded.Sessions), len(tree.Sessions))
	}
	for _, s := range decoded.Sessions {
		assertPrunedJSONSession(t, s.ID, s.Subagents)
	}
}

func assertPrunedJSONSession(t *testing.T, id string, subs []struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}) {
	t.Helper()
	var ids []string
	for _, sub := range subs {
		ids = append(ids, sub.ID)
		if sub.Status == StatusFinished || sub.Status == StatusError {
			t.Errorf("json kept %s subagent %s in %s", sub.Status, sub.ID, id)
		}
	}
	if id == "finished-session-aaaa1111" && (len(ids) != 1 || ids[0] != "fakeagentid002") {
		t.Errorf("json subagents = %+v, want [fakeagentid002]", subs)
	}
}

const (
	dedupeCwd    = "/Users/fake/dedupe-repo"
	dedupeSlug   = "-Users-fake-dedupe-repo"
	headlessPrmt = "Summarize the flaky retry logic in the router and propose a fix"
)

type dedupeEnv struct {
	cfg string
	now time.Time
}

// newDedupeEnv points CLAUDE_CONFIG_DIR at a temp dir holding projects/ and state/harness/dispatch.
func newDedupeEnv(t *testing.T) dedupeEnv {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return dedupeEnv{cfg: cfg, now: time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)}
}

// session writes an active transcript (first user prompt = prompt); withSub adds a running subagent.
func (e dedupeEnv) session(t *testing.T, id, prompt string, withSub bool) {
	t.Helper()
	dir := filepath.Join(e.cfg, "projects", dedupeSlug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"type": "user", "cwd": dedupeCwd, "sessionId": id, "timestamp": e.now.Format(time.RFC3339),
		"message": map[string]any{"role": "user", "content": prompt},
	})
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, e.now, e.now); err != nil {
		t.Fatal(err)
	}
	if !withSub {
		return
	}
	sub := filepath.Join(dir, id, "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"agent-sub1.jsonl":     `{"type":"user","timestamp":"` + e.now.Format(time.RFC3339) + `"}` + "\n",
		"agent-sub1.meta.json": `{"finished":false}`,
	} {
		p := filepath.Join(sub, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, e.now, e.now); err != nil {
			t.Fatal(err)
		}
	}
}

// dispatch saves a live (this pid) running record started a minute before now; returns its id.
func (e dedupeEnv) dispatch(t *testing.T, name, prompt, workDir, repo, sessionID string) string {
	t.Helper()
	req := agent.RunOptions{Prompt: prompt, WorkDir: workDir}
	rec := job.NewRecord(name, req, e.now.Add(-time.Minute))
	rec.PID, rec.Repo, rec.SessionID = os.Getpid(), repo, sessionID
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

func (e dedupeEnv) tree(t *testing.T) Tree {
	t.Helper()
	tree, err := listing(e.cfg, Options{Days: 60, Running: true, Now: pinNow(e.now)}, true)
	if err != nil {
		t.Fatal(err)
	}
	return *tree
}

func hasSession(tree Tree, id string) bool {
	for _, s := range tree.Sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}

// A matching session that has subagents is real work and is never dropped.
func TestRunningKeepsMatchedSessionWithSubagents(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-dddd", headlessPrmt, true)
	e.dispatch(t, "claude", headlessPrmt, dedupeCwd, dedupeCwd, "headless-dddd")
	tree := e.tree(t)
	if !hasSession(tree, "headless-dddd") || len(tree.Dispatches) != 1 {
		t.Errorf("session with subagents must stay: sessions=%d dispatches=%d", len(tree.Sessions), len(tree.Dispatches))
	}
}

// A non-claude dispatch never drops a session, even with a matching cwd and prompt.
func TestRunningCodexDispatchDoesNotDropSession(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-eeee", headlessPrmt, false)
	e.dispatch(t, "codex", headlessPrmt, dedupeCwd, dedupeCwd, "headless-eeee")
	tree := e.tree(t)
	if !hasSession(tree, "headless-eeee") || len(tree.Dispatches) != 1 {
		t.Errorf("codex dispatch must not dedupe: sessions=%d dispatches=%d", len(tree.Sessions), len(tree.Dispatches))
	}
}

// A session last written before the dispatch started (but still active) cannot be its child.
func TestRunningKeepsSessionOlderThanDispatch(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-ffff", headlessPrmt, false)
	old := e.now.Add(-100 * time.Second) // active (<2m) yet before the dispatch start (-60s)
	if err := os.Chtimes(filepath.Join(e.cfg, "projects", dedupeSlug, "headless-ffff.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	e.dispatch(t, "claude", headlessPrmt, dedupeCwd, dedupeCwd, "")
	if !hasSession(e.tree(t), "headless-ffff") {
		t.Error("session older than the dispatch start must stay")
	}
}

// Known gap: WorkDir empty and the job ran in a linked worktree, so Repo (main checkout) != session cwd;
// the dispatch fails safe and stays listed twice.
func TestRunningWorktreeGapStaysListedTwice(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-gggg", headlessPrmt, false)
	e.dispatch(t, "claude", headlessPrmt, "", "/Users/fake/main-checkout", "")
	tree := e.tree(t)
	if !hasSession(tree, "headless-gggg") || len(tree.Dispatches) != 1 {
		t.Errorf("gap case should list twice: sessions=%d dispatches=%d", len(tree.Sessions), len(tree.Dispatches))
	}
}

// New coverage for the journal-backed status, activity, vendor-dispatches and drill-down work
// (plans/2026-10-18-agents-tree-visibility.md Part 1). Written after `go build ./internal/agent`
// succeeded against the implementer's change; dispatched concurrently as the copilot test agent.

// subagentByID finds one subagent inside a session's direct Subagents or any of its Workflow phases.
func subagentByID(s Session, id string) (Subagent, bool) {
	for _, sub := range s.Subagents {
		if sub.ID == id {
			return sub, true
		}
	}
	for _, run := range s.Workflows {
		for _, phase := range run.Phases {
			for _, sub := range phase.Agents {
				if sub.ID == id {
					return sub, true
				}
			}
		}
	}
	return Subagent{}, false
}

// (1) A workflow agent with a journal "result" line is finished, and a null result line is error —
// both regardless of the sidecar meta's own (missing/false) "finished" flag.
func TestJournalDrivesFinishedAndErrorStatus(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")

	gate, ok := subagentByID(s, "fakewfagent002")
	if !ok {
		t.Fatal("fakewfagent002 not found")
	}
	if gate.Status != StatusFinished {
		t.Errorf("fakewfagent002 status = %q, want finished (journal result line, meta has no finished:true)", gate.Status)
	}

	doomed, ok := subagentByID(s, "fakewfagent030")
	if !ok {
		t.Fatal("fakewfagent030 not found")
	}
	if doomed.Status != StatusError {
		t.Errorf("fakewfagent030 status = %q, want error (journal result:null)", doomed.Status)
	}
}

// (2) A journal "started" line with no result yet keeps today's mtime-driven running read.
func TestJournalStartedWithoutResultStillRunning(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	busy, ok := subagentByID(s, "fakewfagent020")
	if !ok {
		t.Fatal("fakewfagent020 not found")
	}
	if busy.Status != StatusRunning {
		t.Errorf("fakewfagent020 status = %q, want running (started, no result, fresh mtime)", busy.Status)
	}
	run := workflowByID(t, s.Workflows, "wf_fake0003-running")
	if run.Status != StatusRunning {
		t.Errorf("wf_fake0003-running run status = %q, want running", run.Status)
	}
}

// (3) A manifest-less workflow run (no workflows/wf_*.json at all) is read entirely from its journal
// and agent transcripts, and reports "finished" once every started agent has a result and the run
// dir has gone quiet past subagentActiveWindow.
func TestManifestLessWorkflowRunFinishesFromJournal(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	// Push this one run's own files outside subagentActiveWindow without touching the rest of the
	// fixture (copyTestdataTree already backdated the whole session tree to within the window).
	runDir := filepath.Join(claudeDir, "projects", "-Users-fake-demo-repo", "finished-session-aaaa1111",
		"subagents", "workflows", "wf_fake0002-nomanifest")
	old := now.Add(-20 * time.Minute)
	chtimesAll(t, runDir, old)

	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	run := workflowByID(t, s.Workflows, "wf_fake0002-nomanifest")
	if run.Name != "wf_fake0002-nomanifest" {
		t.Errorf("run.Name = %q, want the run id (no manifest to name it)", run.Name)
	}
	if run.Status != StatusFinished {
		t.Errorf("wf_fake0002-nomanifest status = %q, want finished (journal started==resulted, quiet past activeWindow)", run.Status)
	}
}

// (6) A relay subagent's Dispatches carry every vendor dispatch its transcript's exit lines named: plain
// ok, still-running, and a fallback (Fallback+FallbackReason set, matched=="" since read directly).
func TestVendorDispatchesDirectMatch(t *testing.T) {
	withDispatchFixture(t)
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	gate, ok := subagentByID(s, "fakewfagent002")
	if !ok {
		t.Fatal("fakewfagent002 not found")
	}
	if len(gate.Dispatches) != 3 {
		t.Fatalf("Dispatches = %+v, want 3 (codex ok, gemini running, codex->claude fallback)", gate.Dispatches)
	}
	assertGateDispatches(t, gate.Dispatches)
}

func assertGateDispatches(t *testing.T, dispatches []Dispatch) {
	t.Helper()
	byID := map[string]Dispatch{}
	for _, l := range dispatches {
		byID[l.ID] = l
	}
	if l := byID["20261017T231131.948-codex"]; l.Status != "ok" || l.Matched != "" {
		t.Errorf("codex dispatch = %+v, want status=ok matched=\"\"", l)
	}
	if l := byID["20261018T095401.709-gemini"]; l.Status != "running" || l.Duration == "" {
		t.Errorf("gemini dispatch = %+v, want status=running with an elapsed duration", l)
	}
	fb := byID["20261018T150000.000-codex"]
	if fb.Fallback != "claude" || fb.FallbackReason == "" {
		t.Errorf("fallback dispatch = %+v, want Fallback=claude with a reason", fb)
	}
}

// (7) A relay whose own transcript hasn't logged its `ask` exit line yet (still mid-dispatch) is
// matched heuristically against the dispatch log's own still-running records: same WorkDir, started
// no earlier than the subagent itself.
func TestVendorDispatchHeuristicRunningMatch(t *testing.T) {
	withDispatchFixture(t)
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByID(t, tree, "finished-session-aaaa1111")
	busy, ok := subagentByID(s, "fakewfagent020")
	if !ok {
		t.Fatal("fakewfagent020 not found")
	}
	var heuristic *Dispatch
	for i := range busy.Dispatches {
		if busy.Dispatches[i].Matched == "heuristic" {
			heuristic = &busy.Dispatches[i]
		}
	}
	if heuristic == nil {
		t.Fatalf("Dispatches = %+v, want one heuristically matched dispatch", busy.Dispatches)
	}
	if heuristic.ID != "20261018T100500.000-gemini" {
		t.Errorf("heuristic dispatch id = %q, want the still-running gemini dispatch", heuristic.ID)
	}
}

const deadPID = 2147483646

// regFixture lays down a registry entry plus its transcript (aged by age) under claudeDir.
func regFixture(t *testing.T, claudeDir, id string, pid int, age time.Duration, now time.Time) {
	t.Helper()
	entry, _ := json.Marshal(regEntry{PID: pid, SessionID: id, Cwd: "/repo", Name: "name-" + id, Status: "waiting", Kind: "interactive"})
	reg := filepath.Join(claudeDir, "sessions", fmt.Sprintf("%d.json", pid))
	tr := filepath.Join(claudeDir, "projects", "-repo", id+".jsonl")
	for p, body := range map[string]string{
		reg: string(entry),
		tr:  `{"type":"user","cwd":"/repo","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mt := now.Add(-age)
	if err := os.Chtimes(tr, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryJoin: live pid beats a stale transcript under --running; a dead pid and an unparsable file add nothing.
func TestRegistryJoin(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	old := pidAlive
	pidAlive = func(pid int) bool { return pid == os.Getpid() }
	t.Cleanup(func() { pidAlive = old })

	regFixture(t, claudeDir, "live-stale", os.Getpid(), 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-stale", deadPID, 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-fresh", deadPID-1, time.Minute, now)
	for name, body := range map[string]string{"junk.json": "{not json", "noid.json": `{"pid":1,"name":"x"}`} {
		if err := os.WriteFile(filepath.Join(claudeDir, "sessions", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tree, err := build(claudeDir, Options{Days: 1, Running: true, Now: pinNow(now)})
	if err != nil {
		t.Fatalf("build with an unparsable registry file: %v", err)
	}
	got := registryJoinedSession(t, tree)
	if !got.Live || got.PID != os.Getpid() || got.Name != "name-live-stale" || got.CLIStatus != "waiting" || got.Mode != "interactive" {
		t.Errorf("registry fields not joined: %+v", got)
	}
}

func registryJoinedSession(t *testing.T, tree *Tree) *Session {
	t.Helper()
	var got *Session
	for i, s := range tree.Sessions {
		switch s.ID {
		case "dead-stale":
			t.Errorf("dead pid with stale transcript listed under --running")
		case "dead-fresh":
			if s.Live || s.PID != 0 {
				t.Errorf("dead pid marked live: %+v", s)
			}
		case "live-stale":
			got = &tree.Sessions[i]
		}
	}
	if got == nil {
		t.Fatal("live pid with a 40-day-old transcript dropped by --running")
	}
	return got
}

// TestReadRegistrySkipsUnusableEntries: a live pid without a sessionId and an unparsable file yield no entry.
func TestReadRegistrySkipsUnusableEntries(t *testing.T) {
	claudeDir := t.TempDir()
	old := pidAlive
	pidAlive = func(int) bool { return true }
	t.Cleanup(func() { pidAlive = old })
	files := map[string]string{"junk.json": "{not json", "noid.json": `{"pid":7,"name":"x"}`, "ok.json": `{"pid":8,"sessionId":"s8"}`}
	for name, body := range files {
		p := filepath.Join(claudeDir, "sessions", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := readRegistry(claudeDir); len(got) != 1 || got["s8"].PID != 8 {
		t.Errorf("readRegistry = %+v, want only s8", got)
	}
}
