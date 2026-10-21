package agenttree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if killed.Name != "killed-run" || killed.Status != statusError {
		t.Errorf("killed run = %+v, want name=killed-run status=%s", killed, statusError)
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
	if got := byID["fakeagentid001"].Status; got != statusFinished {
		t.Errorf("agent001 (finished:true) status = %q, want finished", got)
	}
	if got := byID["fakeagentid002"].Status; got != statusRunning {
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
		if sub.ID == "fakeagentid002" && sub.Status != statusStale {
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

// (g) RenderText never produces a line over 100 columns, even for a subagent with a long description.
func TestRenderTextLineWidth(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := Build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	RenderText(&buf, tree, now)
	// max is terminalWidth()'s own fallback, not a hardcoded guess: the render change added a status
	// glyph + idle column and widened defaultWidth (100 -> 120) to fit them.
	max := terminalWidth()
	for i, line := range strings.Split(buf.String(), "\n") {
		if n := len([]rune(line)); n > max {
			t.Errorf("line %d is %d columns (max %d): %q", i, n, max, line)
		}
	}
	if !strings.Contains(buf.String(), "/Users/fake/demo-repo") {
		t.Error("expected the repo's cwd to head its group")
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
	var buf strings.Builder
	RenderText(&buf, tree, now)
	if !strings.Contains(buf.String(), "malformed-session-dddd4444"[:8]) {
		t.Error("expected the malformed session's short id in the rendered text")
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
