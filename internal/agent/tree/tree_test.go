package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

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
	src := filepath.Join("testdata", "projects")
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(claudeDir, "projects", rel)
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

	// Backdate every fixture to its scenario's intended age relative to `now`: finished-session and its
	// subagents/workflow stay just inside the active window so Build's --days scoping always finds them;
	// running-session is fresh; stale-session is far outside both the activeWindow and a 1-day --days scope.
	mtimes := map[string]time.Time{
		"finished-session-aaaa1111.jsonl":  now.Add(-5 * time.Minute),
		"running-session-bbbb2222.jsonl":   now,
		"stale-session-cccc3333.jsonl":     now.Add(-30 * 24 * time.Hour),
		"malformed-session-dddd4444.jsonl": now.Add(-time.Minute),
	}
	proj := filepath.Join(claudeDir, "projects", "-Users-fake-demo-repo")
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
	return claudeDir, now
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
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	if got := byID["fakeagentid001"].Type; got != "code-workers" {
		t.Errorf("agent001 type = %q, want code-workers", got)
	}
}

// (a continued) The session's one workflow run folds its two agent transcripts into their declared
// phases (Detect, Gate), each phase holding the agents that ran inside it.
func TestBuildFoldsWorkflowRunIntoPhases(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	phases := map[string][]string{}
	for _, p := range run.Phases {
		for _, a := range p.Agents {
			phases[p.Name] = append(phases[p.Name], a.ID)
		}
	}
	if got := phases["Detect"]; len(got) != 1 || got[0] != "fakewfagent001" {
		t.Errorf("Detect phase agents = %v, want [fakewfagent001]", got)
	}
	if got := phases["Gate"]; len(got) != 1 || got[0] != "fakewfagent002" {
		t.Errorf("Gate phase agents = %v, want [fakewfagent002]", got)
	}
}

// (b) Status classification: a session/subagent written within activeWindow of the pinned clock is
// active/running; finished wins outright for a subagent regardless of mtime; otherwise idle/stale.
func TestStatusClassificationUsesPinnedClock(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Days: 1, Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Session: "stale-session-cccc3333", Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
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

// (f) --format json's shape round-trips through the documented schema: {sessions:[{id, project, cwd,
// branch, title, updated, status, tokens, subagents:[...], workflows:[...]}]}.
func TestBuildJSONSchema(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := Build(claudeDir, Options{Session: "finished-session-aaaa1111", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Sessions []struct {
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
				ID     string `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Phases []struct {
					Name   string `json:"name"`
					Agents []struct {
						ID string `json:"id"`
					} `json:"agents"`
				} `json:"phases"`
			} `json:"workflows"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal into documented schema: %v\nraw: %s", err, raw)
	}
	if len(decoded.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(decoded.Sessions))
	}
	s := decoded.Sessions[0]
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
	var mainRun struct {
		Phases []struct {
			Name   string `json:"name"`
			Agents []struct {
				ID string `json:"id"`
			} `json:"agents"`
		}
	}
	for _, r := range s.Workflows {
		if r.ID == "wf_fake0001-abc" {
			mainRun.Phases = r.Phases
		}
	}
	if len(mainRun.Phases) != 2 {
		t.Errorf("wf_fake0001-abc phases = %+v, want 2", mainRun.Phases)
	}
}

// (h) A session whose last transcript line is truncated/malformed still renders without panicking;
// the earlier valid lines still populate cwd/branch/title.
func TestMalformedLastLineDoesNotPanic(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Build panicked on a malformed last line: %v", r)
		}
	}()
	tree, err := Build(claudeDir, Options{Session: "malformed-session-dddd4444", Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Session: "finished-session-aaaa1111", Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Session: "running-session-bbbb2222", Now: pinNow(now)})
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
		if _, err := Build(claudeDir, Options{Session: bad, Now: pinNow(now)}); err == nil {
			t.Errorf("Build(Session=%q) = nil error, want rejection of a non-plain session id", bad)
		}
	}
}

// buildWorkflowRuns ties two runs with identical Updated timestamps by ID ascending, so a stable
// sort doesn't just fall back to map-iteration (nondeterministic) order.
func TestBuildWorkflowRunsTieBreaksByID(t *testing.T) {
	sessionDir := t.TempDir()
	workflowsDir := filepath.Join(sessionDir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := "2026-10-18T03:00:00Z"
	for _, id := range []string{"run-zzz", "run-aaa"} {
		manifest := map[string]string{"runId": id, "timestamp": ts, "status": "completed"}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workflowsDir, "wf_"+id+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	runs := buildWorkflowRuns(sessionDir, now, "")
	if len(runs) != 2 {
		t.Fatalf("got %d workflow runs, want 2", len(runs))
	}
	if runs[0].ID != "run-aaa" || runs[1].ID != "run-zzz" {
		t.Errorf("got order [%s, %s], want [run-aaa, run-zzz] (ID-ascending tie-break on equal Updated)", runs[0].ID, runs[1].ID)
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
	tree, err := Build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
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
	for _, r := range s.Workflows {
		for _, p := range r.Phases {
			for _, a := range p.Agents {
				if a.Status == StatusFinished || a.Status == StatusError {
					t.Errorf("--running kept %s workflow agent %s", a.Status, a.ID)
				}
			}
		}
	}
	found := false
	for _, id := range runs {
		found = found || id == "wf_fake0003-running"
	}
	if !found {
		t.Errorf("running workflow run wf_fake0003-running dropped (runs=%v)", runs)
	}
}

// Plain Build (no --running) keeps both the finished and the running subagent.
func TestBuildPlainKeepsFinishedChildren(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Days: 60, Running: true, Now: pinNow(now)})
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
		for _, sub := range s.Subagents {
			if sub.Status == StatusFinished || sub.Status == StatusError {
				t.Errorf("json kept %s subagent %s in %s", sub.Status, sub.ID, s.ID)
			}
		}
		if s.ID == "finished-session-aaaa1111" && (len(s.Subagents) != 1 || s.Subagents[0].ID != "fakeagentid002") {
			t.Errorf("json subagents = %+v, want [fakeagentid002]", s.Subagents)
		}
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
	rec := agent.NewRecord(name, req, e.now.Add(-time.Minute))
	rec.PID, rec.Repo, rec.SessionID = os.Getpid(), repo, sessionID
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

func (e dedupeEnv) tree(t *testing.T) Tree {
	t.Helper()
	tree, err := Listing(e.cfg, Options{Days: 60, Running: true, Now: pinNow(e.now)}, true)
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

// A headless claude dispatch matched by cwd + time + prompt renders once, as its dispatch row;
// an unrelated session in the same dir (different prompt) stays.
func TestRunningDedupesHeadlessClaudeByPrompt(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-aaaa", headlessPrmt, false)
	e.session(t, "human-bbbb", "Add a new feature to the statusline", false)
	id := e.dispatch(t, "claude", headlessPrmt, dedupeCwd, dedupeCwd, "")

	tree := e.tree(t)
	if hasSession(tree, "headless-aaaa") {
		t.Error("matched headless session should be dropped from sessions")
	}
	if !hasSession(tree, "human-bbbb") {
		t.Error("unrelated session in the same dir must not be dropped")
	}
	if len(tree.Dispatches) != 1 || tree.Dispatches[0].ID != id {
		t.Errorf("dispatches = %+v, want only %s", tree.Dispatches, id)
	}
}

// The record's SessionID alone matches, even when the prompt differs.
func TestRunningDedupesBySessionID(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-cccc", "something else entirely", false)
	e.dispatch(t, "claude", headlessPrmt, "", "/elsewhere", "headless-cccc")
	if hasSession(e.tree(t), "headless-cccc") {
		t.Error("session whose id equals the dispatch SessionID should be dropped")
	}
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
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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

	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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

// (4) lastActivity's Arg strips both the session cwd and $HOME, for each tool shape the plan names:
// Read/file_path, Edit/file_path, Bash/command (truncated), Grep/pattern.
func TestLastActivityStripsHomeAndCwdPerTool(t *testing.T) {
	// Fixed, short strings (not t.TempDir()) so Bash's command truncation (60 runes) never clips the
	// path before stripPaths gets to it — toolArg truncates, then lastActivity strips what remains.
	const home = "/Users/fakehome"
	const cwd = "/Users/fake/demo-repo"
	t.Setenv("HOME", home)

	cases := []struct {
		name  string
		tool  string
		input string // raw JSON "input" object
		want  string
	}{
		{"Read", "Read", `{"file_path":"` + cwd + `/internal/agenttree/scan.go"}`, "internal/agenttree/scan.go"},
		// $HOME (not cwd) appears here, so stripPaths should collapse it to "~".
		{"Edit", "Edit", `{"file_path":"` + home + `/.claude/CLAUDE.md"}`, "~/.claude/CLAUDE.md"},
		{"Bash", "Bash", `{"command":"go test ` + cwd + `/internal/agenttree/..."}`, "go test internal/agenttree/..."},
		// No "path" field: toolArg prefers file_path/path over pattern, so this exercises the Pattern branch.
		{"Grep", "Grep", `{"pattern":"readJournal mentioned in ` + cwd + `"}`, "readJournal mentioned in "},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent-x.jsonl")
			line := `{"parentUuid":null,"isSidechain":true,"agentId":"x","type":"assistant","timestamp":"2026-10-18T03:00:00Z",` +
				`"message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"` + c.tool + `","input":` + c.input + `}]}}`
			if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			act := lastActivity(path, cwd)
			if act.Tool != c.tool {
				t.Errorf("Tool = %q, want %q", act.Tool, c.tool)
			}
			if act.Arg != c.want {
				t.Errorf("Arg = %q, want %q", act.Arg, c.want)
			}
			if act.At.IsZero() {
				t.Error("At should be the record's own timestamp, not zero")
			}
		})
	}
}

// (5) dispatchIDs greps every "ask: dispatch <id>" exit line out of a relay transcript, deduped, in
// the order they first appear.
func TestDispatchIDsExtraction(t *testing.T) {
	path := filepath.Join("testdata", "projects", "-Users-fake-demo-repo", "finished-session-aaaa1111",
		"subagents", "workflows", "wf_fake0001-abc", "agent-fakewfagent002.jsonl")
	ids := dispatchIDs(path)
	want := []string{"20261017T231131.948-codex", "20261018T095401.709-gemini", "20261018T150000.000-codex"}
	if len(ids) != len(want) {
		t.Fatalf("dispatchIDs = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

// withDispatchFixture points internal/agent's dispatch log at testdata/dispatch for the duration of
// the test, via CLAUDE_CONFIG_DIR (internal/paths.StateDir honors it).
func withDispatchFixture(t *testing.T) {
	t.Helper()
	cfgDir := t.TempDir()
	dst := filepath.Join(cfgDir, "state", "harness", "dispatch")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("testdata", "dispatch")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
}

// (6) A relay subagent's Dispatches carry every vendor dispatch its transcript's exit lines named: plain
// ok, still-running, and a fallback (Fallback+FallbackReason set, matched=="" since read directly).
func TestVendorDispatchesDirectMatch(t *testing.T) {
	withDispatchFixture(t)
	claudeDir, now := copyTestdataTree(t)
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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
	byID := map[string]Dispatch{}
	for _, l := range gate.Dispatches {
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
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
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

// (8) View's Snapshot carries the edited files, dispatches and a result summary for a finished workflow
// agent, and the event stream is ordered oldest-first.
func TestViewSnapshotFields(t *testing.T) {
	withDispatchFixture(t)
	claudeDir, now := copyTestdataTree(t)
	snap, err := View(ViewOptions{ClaudeDir: claudeDir, Session: "finished-session-aaaa1111", Agent: "fakewfagent002", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	if snap.ID != "fakewfagent002" || snap.Workflow != "wf_fake0001-abc" || snap.Phase != "Gate" {
		t.Errorf("snapshot id/workflow/phase = %q/%q/%q", snap.ID, snap.Workflow, snap.Phase)
	}
	if snap.Status != StatusFinished {
		t.Errorf("snapshot status = %q, want finished", snap.Status)
	}
	wantEdited := []string{"/Users/fake/demo-repo/internal/agenttree/scan.go"}
	if len(snap.FilesEdited) != 1 || snap.FilesEdited[0] != wantEdited[0] {
		t.Errorf("FilesEdited = %v, want %v", snap.FilesEdited, wantEdited)
	}
	if len(snap.Dispatches) != 3 {
		t.Errorf("Dispatches = %+v, want 3", snap.Dispatches)
	}
	if snap.Result != "green." {
		t.Errorf("Result = %q, want the journal result preview %q", snap.Result, "green.")
	}
	for i := 1; i < len(snap.Events); i++ {
		if snap.Events[i].At.Before(snap.Events[i-1].At) {
			t.Fatalf("events not ordered oldest-first at index %d: %+v", i, snap.Events)
		}
	}
	if len(snap.Events) == 0 {
		t.Error("expected at least one event from fakewfagent002's transcript")
	}
}

// (9) View's --n keeps only the trailing N events read from the transcript; a terminal journal
// result (fakewfagent002 has one) is appended on top of that and is never itself capped by --n.
func TestViewSnapshotEventCountN(t *testing.T) {
	withDispatchFixture(t)
	claudeDir, now := copyTestdataTree(t)
	full, err := View(ViewOptions{ClaudeDir: claudeDir, Session: "finished-session-aaaa1111", Agent: "fakewfagent002", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Events) < 3 || full.Events[len(full.Events)-1].Kind != "result" {
		t.Fatalf("precondition: need >=3 events ending in a result, got %+v", full.Events)
	}
	lastRealEvent := full.Events[len(full.Events)-2]

	limited, err := View(ViewOptions{ClaudeDir: claudeDir, Session: "finished-session-aaaa1111", Agent: "fakewfagent002", N: 1, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	// 1 kept transcript event + the appended result event.
	if len(limited.Events) != 2 {
		t.Fatalf("got %d events with --n 1, want 2 (1 kept + the result): %+v", len(limited.Events), limited.Events)
	}
	if limited.Events[0] != lastRealEvent {
		t.Errorf("--n 1 kept event = %+v, want the newest transcript event %+v", limited.Events[0], lastRealEvent)
	}
	if limited.Events[1].Kind != "result" {
		t.Errorf("limited.Events[1].Kind = %q, want result", limited.Events[1].Kind)
	}
}

// (10) Follow emits one line per new event and stops as soon as the journal carries a result line.
func TestFollowStopsOnJournalResult(t *testing.T) {
	withDispatchFixture(t)
	claudeDir, now := copyTestdataTree(t)
	var buf strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- Follow(context.Background(), ViewOptions{ClaudeDir: claudeDir, Session: "finished-session-aaaa1111", Agent: "fakewfagent002", Now: pinNow(now)}, func(e Event) { fmt.Fprintln(&buf, FormatEvent(e)) })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not stop within 5s of an already-resulted journal entry")
	}
	if !strings.Contains(buf.String(), "result") {
		t.Errorf("Follow output = %q, want a result line once the journal result appears", buf.String())
	}
}

// Regression: the pre-change JSON schema (lastTool, no activity/dispatches) still decodes a tree built
// after the change — new fields are additive (omitempty), never required.
func TestJSONSchemaStillDecodesPreChangeShape(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := Build(claudeDir, Options{Session: "finished-session-aaaa1111", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Sessions []struct {
			Subagents []struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				LastTool string `json:"lastTool"`
			} `json:"subagents"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("pre-change schema failed to decode post-change JSON: %v\nraw: %s", err, raw)
	}
	if len(old.Sessions) != 1 || len(old.Sessions[0].Subagents) != 2 {
		t.Fatalf("decoded = %+v", old)
	}
}
