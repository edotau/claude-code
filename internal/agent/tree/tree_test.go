package tree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude" // the vendors register their roster rows from their own init
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/hosttest"
)

const (
	finishedSID = "finished-session-aaaa1111"
	staleSID    = "stale-session-cccc3333"
	runningSID  = "running-session-bbbb2222"
	demoProj    = "-Users-fake-demo-repo"
)

var (
	sharedOnce sync.Once
	sharedDir  string
	sharedNow  time.Time
)

// TestMain runs in the hermetic host (hosttest) and removes the read-only fixture shared by the tests below.
func TestMain(m *testing.M) {
	code := hosttest.Main(m)
	if sharedDir != "" {
		os.RemoveAll(sharedDir)
	}
	os.Exit(code)
}

// pinNow returns Options.Now pinned to t, so status classification never depends on wall-clock time.
func pinNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// copyTestdataTree copies testdata/projects into a fresh temp claudeDir with each transcript's mtime backdated.
func copyTestdataTree(t *testing.T) (claudeDir string, now time.Time) {
	t.Helper()
	claudeDir = t.TempDir()
	return claudeDir, fillFixture(t, claudeDir)
}

func fillFixture(t *testing.T, claudeDir string) time.Time {
	t.Helper()
	now := time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	src, dstRoot := filepath.Join("testdata", "projects"), filepath.Join(claudeDir, "projects")
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
	proj := filepath.Join(dstRoot, demoProj)
	for name, mt := range map[string]time.Time{
		finishedSID + ".jsonl":             now.Add(-5 * time.Minute),
		runningSID + ".jsonl":              now,
		staleSID + ".jsonl":                now.Add(-30 * 24 * time.Hour),
		"malformed-session-dddd4444.jsonl": now.Add(-time.Minute),
	} {
		touch(t, filepath.Join(proj, name), mt)
	}
	// agent-fakeagentid002 (unfinished) must sit inside activeWindow to classify "running".
	chtimesAll(t, filepath.Join(proj, finishedSID), now.Add(-5*time.Minute))
	touch(t, agent002Path(proj), now.Add(-time.Minute))
	return now
}

func agent002Path(proj string) string {
	return filepath.Join(proj, finishedSID, "subagents", "agent-fakeagentid002.jsonl")
}

func touch(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// sharedFixture returns one read-only fixture claudeDir for every test that never mutates it.
func sharedFixture(t *testing.T) (string, time.Time) {
	t.Helper()
	sharedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "tree-fixture")
		if err != nil {
			t.Fatal(err)
		}
		sharedDir, sharedNow = dir, fillFixture(t, dir)
	})
	return sharedDir, sharedNow
}

// buildShared builds the shared fixture under the pinned clock; opts.Now is set here.
func buildShared(t *testing.T, opts Options) *Tree {
	t.Helper()
	dir, now := sharedFixture(t)
	opts.Now = pinNow(now)
	tree, err := build(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return tree
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

func mustSub(t *testing.T, s Session, id string) Subagent {
	t.Helper()
	sub, ok := subagentByID(s, id)
	if !ok {
		t.Fatalf("%s not found", id)
	}
	return sub
}

func sessionIDs(tree *Tree) map[string]bool {
	ids := map[string]bool{}
	for _, s := range tree.Sessions {
		ids[s.ID] = true
	}
	return ids
}

// phaseAgents maps phase name to the agent ids it holds; wfAgents lists every workflow agent.
func phaseAgents(run WorkflowRun) map[string][]string {
	phases := map[string][]string{}
	for _, p := range run.Phases {
		for _, a := range p.Agents {
			phases[p.Name] = append(phases[p.Name], a.ID)
		}
	}
	return phases
}

// TestBuildFixtureSession reads the finished session once; its subtests share that one tree.
func TestBuildFixtureSession(t *testing.T) {
	t.Parallel()
	tree := buildShared(t, Options{Days: 60})
	s := sessionByID(t, tree, finishedSID)
	subs := func(t *testing.T) map[string]Subagent {
		byID := map[string]Subagent{}
		for _, sub := range s.Subagents {
			byID[sub.ID] = sub
		}
		return byID
	}

	t.Run("subagents by depth", func(t *testing.T) {
		t.Parallel()
		byID := subs(t)
		if len(s.Subagents) != 2 {
			t.Fatalf("got %d subagents, want 2: %+v", len(s.Subagents), s.Subagents)
		}
		if byID["fakeagentid001"].Depth != 1 || byID["fakeagentid002"].Depth != 2 || byID["fakeagentid001"].Type != "copilot" {
			t.Errorf("agent001 = %+v, agent002 = %+v, want depth 1/2 and type copilot", byID["fakeagentid001"], byID["fakeagentid002"])
		}
	})
	t.Run("workflow phases", func(t *testing.T) {
		t.Parallel()
		// wf_fake0001 manifest, 0002 manifest-less finished, 0003 manifest-less running, 0004 error, 0005 killed.
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
		phases := phaseAgents(run)
		for name, id := range map[string]string{"Detect": "fakewfagent001", "Gate": "fakewfagent002"} {
			if got := phases[name]; len(got) != 1 || got[0] != id {
				t.Errorf("%s phase agents = %v, want [%s]", name, got, id)
			}
		}
	})
	t.Run("status uses the pinned clock", func(t *testing.T) {
		t.Parallel()
		if got := sessionByID(t, tree, runningSID).Status; got != statusActive {
			t.Errorf("running-session status = %q, want active", got)
		}
		if got := sessionByID(t, tree, staleSID).Status; got != statusIdle {
			t.Errorf("stale-session status = %q, want idle", got)
		}
		byID := subs(t)
		if got := byID["fakeagentid001"].Status; got != statusFinished {
			t.Errorf("agent001 (finished:true) status = %q, want finished", got)
		}
		if got := byID["fakeagentid002"].Status; got != statusRunning {
			t.Errorf("agent002 (finished:false, fresh mtime) status = %q, want running", got)
		}
	})
	t.Run("cache read is separate from in", func(t *testing.T) {
		t.Parallel()
		// Fixture usage: input_tokens=2, cache_creation_input_tokens=27907, cache_read_input_tokens=0.
		a := subs(t)["fakeagentid001"]
		if a.Tokens.In != 2+27907 || a.Tokens.CacheRead != 0 {
			t.Errorf("agent001 Tokens = %+v, want In=%d (input+cache-creation) CacheRead=0", a.Tokens, 2+27907)
		}
		raw, err := json.Marshal(s.Tokens)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"cacheRead"`) {
			t.Errorf("Tokens JSON missing cacheRead key: %s", raw)
		}
	})
	t.Run("journal result drives finished and error", func(t *testing.T) {
		t.Parallel()
		if got := mustSub(t, s, "fakewfagent002").Status; got != statusFinished {
			t.Errorf("fakewfagent002 status = %q, want finished (journal result line, meta has no finished:true)", got)
		}
		if got := mustSub(t, s, "fakewfagent030").Status; got != statusError {
			t.Errorf("fakewfagent030 status = %q, want error (journal result:null)", got)
		}
	})
	t.Run("journal started without result stays running", func(t *testing.T) {
		t.Parallel()
		if got := mustSub(t, s, "fakewfagent020").Status; got != statusRunning {
			t.Errorf("fakewfagent020 status = %q, want running (started, no result, fresh mtime)", got)
		}
		if got := workflowByID(t, s.Workflows, "wf_fake0003-running").Status; got != statusRunning {
			t.Errorf("wf_fake0003-running run status = %q, want running", got)
		}
	})
	t.Run("plain build keeps finished children", func(t *testing.T) {
		t.Parallel()
		if len(s.Subagents) != 2 || len(s.Workflows) != 5 {
			t.Errorf("plain subagents=%d runs=%d, want 2 and 5", len(s.Subagents), len(s.Workflows))
		}
	})
	t.Run("filterIdleSessions drops only empty idle", func(t *testing.T) {
		t.Parallel()
		kept := filterIdleSessions(tree.Sessions)
		ids := map[string]bool{}
		for _, k := range kept {
			ids[k.ID] = true
		}
		if ids[staleSID] {
			t.Error("stale-session-cccc3333 (idle, no subagents/workflows) should be dropped")
		}
		for _, want := range []string{runningSID, finishedSID, "malformed-session-dddd4444"} {
			if !ids[want] {
				t.Errorf("%s should survive filterIdleSessions (active, or has subagents/workflows)", want)
			}
		}
		if len(kept) != len(tree.Sessions)-1 {
			t.Errorf("got %d kept sessions, want %d (one idle-empty dropped)", len(kept), len(tree.Sessions)-1)
		}
	})
}

func TestBuildFilters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		opts     Options
		has, not []string
		onlyIDs  []string
	}{
		{"days excludes old sessions", Options{Days: 1}, []string{finishedSID}, []string{staleSID}, nil},
		{"session narrows to one id ignoring days", Options{Session: staleSID}, nil, nil, []string{staleSID}},
		{"running keeps active or running-child sessions", Options{Days: 60, Running: true},
			[]string{runningSID, finishedSID}, []string{staleSID}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tree := buildShared(t, c.opts)
			ids := sessionIDs(tree)
			for _, id := range c.has {
				if !ids[id] {
					t.Errorf("%s should survive %+v", id, c.opts)
				}
			}
			for _, id := range c.not {
				if ids[id] {
					t.Errorf("%s should be excluded by %+v", id, c.opts)
				}
			}
			if c.onlyIDs != nil && (len(tree.Sessions) != 1 || tree.Sessions[0].ID != c.onlyIDs[0]) {
				t.Errorf("got %d sessions, want exactly %v: %+v", len(tree.Sessions), c.onlyIDs, tree.Sessions)
			}
		})
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

// --format json round-trips through the documented schema {sessions:[{id, project, cwd, branch, ...}]}.
func TestBuildJSONSchema(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(buildShared(t, Options{Session: finishedSID}))
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
	s := decoded.Sessions[0]
	if s.ID != finishedSID || s.Project != demoProj || s.Cwd != "/Users/fake/demo-repo" || s.Branch != "main" || s.Status != "idle" {
		t.Errorf("session fields = %+v", s)
	}
	if len(s.Subagents) != 2 {
		t.Errorf("got %d subagents in JSON, want 2", len(s.Subagents))
	}
	if len(s.Workflows) != 5 {
		t.Fatalf("got %d workflows, want 5: %+v", len(s.Workflows), s.Workflows)
	}
	for _, r := range s.Workflows {
		if r.ID == "wf_fake0001-abc" && len(r.Phases) != 2 {
			t.Errorf("wf_fake0001-abc phases = %+v, want 2", r.Phases)
		}
	}
}

// A truncated last transcript line must not panic; the earlier valid lines still populate cwd/branch.
func TestMalformedLastLineDoesNotPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("build panicked on a malformed last line: %v", r)
		}
	}()
	s := sessionByID(t, buildShared(t, Options{Session: "malformed-session-dddd4444"}), "malformed-session-dddd4444")
	if s.Cwd != "/Users/fake/demo-repo" || s.Branch != "main" {
		t.Errorf("session from earlier valid lines = %+v", s)
	}
}

// An active session with no subagents or workflows is not idle, so filterIdleSessions keeps it.
func TestFilterIdleSessionsKeepsActiveEvenWhenEmpty(t *testing.T) {
	t.Parallel()
	tree := buildShared(t, Options{Session: runningSID})
	s := sessionByID(t, tree, runningSID)
	if len(s.Subagents) != 0 || len(s.Workflows) != 0 {
		t.Fatalf("precondition: running-session-bbbb2222 fixture must have no subagents/workflows, got %+v", s)
	}
	if kept := filterIdleSessions(tree.Sessions); len(kept) != 1 {
		t.Fatalf("got %d sessions after filterIdleSessions, want 1 (active session kept)", len(kept))
	}
}

// --running prunes finished/error children of a kept session; the JSON view marshals the same pruned tree.
func TestBuildRunningPrunesFinishedChildren(t *testing.T) {
	t.Parallel()
	tree := buildShared(t, Options{Days: 60, Running: true})

	t.Run("tree", func(t *testing.T) {
		t.Parallel()
		s := sessionByID(t, tree, finishedSID)
		if len(s.Subagents) != 1 || s.Subagents[0].ID != "fakeagentid002" {
			t.Errorf("subagents = %+v, want only the running [fakeagentid002]", s.Subagents)
		}
		var runs []string
		for _, r := range s.Workflows {
			runs = append(runs, r.ID)
			for _, p := range r.Phases {
				for _, a := range p.Agents {
					if a.Status == statusFinished || a.Status == statusError {
						t.Errorf("--running kept %s workflow agent %s", a.Status, a.ID)
					}
				}
			}
		}
		for _, id := range runs {
			if id == "wf_fake0001-abc" || id == "wf_fake0005-killed" {
				t.Errorf("--running kept wholly finished/killed run %s (runs=%v)", id, runs)
			}
		}
		if !slices.Contains(runs, "wf_fake0003-running") {
			t.Errorf("running workflow run wf_fake0003-running dropped (runs=%v)", runs)
		}
	})
	t.Run("json", func(t *testing.T) {
		t.Parallel()
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
			var ids []string
			for _, sub := range s.Subagents {
				ids = append(ids, sub.ID)
				if sub.Status == statusFinished || sub.Status == statusError {
					t.Errorf("json kept %s subagent %s in %s", sub.Status, sub.ID, s.ID)
				}
			}
			if s.ID == finishedSID && (len(ids) != 1 || ids[0] != "fakeagentid002") {
				t.Errorf("json subagents = %+v, want [fakeagentid002]", s.Subagents)
			}
		}
	})
}

// An unfinished subagent past activeWindow is stale; --running keeps it because it may be hung, not done.
func TestSubagentGoesStaleOutsideActiveWindow(t *testing.T) {
	t.Parallel()
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			t.Parallel()
			claudeDir, now := copyTestdataTree(t)
			touch(t, agent002Path(filepath.Join(claudeDir, "projects", demoProj)), now.Add(-time.Hour))
			tree, err := build(claudeDir, Options{Days: 60, Running: running, Now: pinNow(now)})
			if err != nil {
				t.Fatal(err)
			}
			s := sessionByID(t, tree, finishedSID)
			if running {
				if len(s.Subagents) != 1 || s.Subagents[0].ID != "fakeagentid002" || s.Subagents[0].Status != statusStale {
					t.Errorf("subagents = %+v, want the stale fakeagentid002 kept", s.Subagents)
				}
				return
			}
			if got := mustSub(t, s, "fakeagentid002").Status; got != statusStale {
				t.Errorf("agent002 status = %q, want stale", got)
			}
		})
	}
}

// A manifest-less run is read from its journal and finished once every started agent has a result and the dir is quiet.
func TestManifestLessWorkflowRunFinishesFromJournal(t *testing.T) {
	t.Parallel()
	claudeDir, now := copyTestdataTree(t)
	chtimesAll(t, filepath.Join(claudeDir, "projects", demoProj, finishedSID, "subagents", "workflows",
		"wf_fake0002-nomanifest"), now.Add(-20*time.Minute))
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	run := workflowByID(t, sessionByID(t, tree, finishedSID).Workflows, "wf_fake0002-nomanifest")
	if run.Name != "wf_fake0002-nomanifest" {
		t.Errorf("run.Name = %q, want the run id (no manifest to name it)", run.Name)
	}
	if run.Status != statusFinished {
		t.Errorf("wf_fake0002-nomanifest status = %q, want finished (journal started==resulted, quiet past activeWindow)", run.Status)
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

// writeFile creates path's directory and writes body, optionally setting mtime when mt is non-zero.
func writeFile(t *testing.T, path, body string, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mt.IsZero() {
		touch(t, path, mt)
	}
}

// session writes a transcript last written at mt (first user prompt = prompt); withSub adds a running subagent.
func (e dedupeEnv) session(t *testing.T, id, prompt string, withSub bool, mt time.Time) {
	t.Helper()
	line, _ := json.Marshal(map[string]any{
		"type": "user", "cwd": dedupeCwd, "sessionId": id, "timestamp": e.now.Format(time.RFC3339),
		"message": map[string]any{"role": "user", "content": prompt},
	})
	dir := filepath.Join(e.cfg, "projects", dedupeSlug)
	writeFile(t, filepath.Join(dir, id+".jsonl"), string(line)+"\n", mt)
	if !withSub {
		return
	}
	sub := filepath.Join(dir, id, "subagents")
	writeFile(t, filepath.Join(sub, "agent-sub1.jsonl"), `{"type":"user","timestamp":"`+e.now.Format(time.RFC3339)+`"}`+"\n", e.now)
	writeFile(t, filepath.Join(sub, "agent-sub1.meta.json"), `{"finished":false}`, e.now)
}

// dispatch saves a live (this pid) running record started a minute before now.
func (e dedupeEnv) dispatch(t *testing.T, name, workDir, repo, sessionID string) {
	t.Helper()
	rec := agent.NewRecord(name, agent.RunOptions{Prompt: headlessPrmt, WorkDir: workDir}, e.now.Add(-time.Minute))
	rec.PID, rec.Repo, rec.SessionID = os.Getpid(), repo, sessionID
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestRunningDedupe(t *testing.T) {
	cases := []struct {
		name                      string
		sid, agent, workDir, repo string
		jobSID                    string
		withSub                   bool
		age                       time.Duration
		checkDispatches           bool
	}{
		{"keeps matched session with subagents", "headless-dddd", "claude", dedupeCwd, dedupeCwd, "headless-dddd", true, 0, true},
		{"codex dispatch never drops a session", "headless-eeee", "codex", dedupeCwd, dedupeCwd, "headless-eeee", false, 0, true},
		// active (<2m) yet written before the dispatch start (-60s), so it cannot be the dispatch's child
		{"keeps session older than dispatch", "headless-ffff", "claude", dedupeCwd, dedupeCwd, "", false, 100 * time.Second, false},
		// WorkDir empty in a linked worktree: Repo (main checkout) != session cwd, so it fails safe and lists twice
		{"worktree gap stays listed twice", "headless-gggg", "claude", "", "/Users/fake/main-checkout", "", false, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDedupeEnv(t)
			e.session(t, c.sid, headlessPrmt, c.withSub, e.now.Add(-c.age))
			e.dispatch(t, c.agent, c.workDir, c.repo, c.jobSID)
			tree, err := listing(e.cfg, Options{Days: 60, Running: true, Now: pinNow(e.now)}, true)
			if err != nil {
				t.Fatal(err)
			}
			if !sessionIDs(tree)[c.sid] || (c.checkDispatches && len(tree.Dispatches) != 1) {
				t.Errorf("session must stay and dispatch be listed: sessions=%d dispatches=%d", len(tree.Sessions), len(tree.Dispatches))
			}
		})
	}
}

// Vendor dispatches come from the dispatch log fixture; both subtests read the same shared tree.
func TestVendorDispatches(t *testing.T) {
	withDispatchFixture(t)
	s := sessionByID(t, buildShared(t, Options{Days: 60}), finishedSID)

	t.Run("direct match", func(t *testing.T) {
		gate := mustSub(t, s, "fakewfagent002")
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
		if fb := byID["20261018T150000.000-codex"]; fb.Fallback != "claude" || fb.FallbackReason == "" {
			t.Errorf("fallback dispatch = %+v, want Fallback=claude with a reason", fb)
		}
	})
	t.Run("heuristic running match", func(t *testing.T) {
		busy := mustSub(t, s, "fakewfagent020")
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
	})
}

const deadPID = 2147483646

// stubPidAlive swaps the pidAlive seam for the test's duration.
func stubPidAlive(t *testing.T, fn func(int) bool) {
	t.Helper()
	old := pidAlive
	pidAlive = fn
	t.Cleanup(func() { pidAlive = old })
}

// regFixture lays down a registry entry plus its transcript (aged by age) under claudeDir.
func regFixture(t *testing.T, claudeDir, id string, pid int, age time.Duration, now time.Time) {
	t.Helper()
	entry, _ := json.Marshal(regEntry{PID: pid, SessionID: id, Cwd: "/repo", Name: "name-" + id, Status: "waiting", Kind: "interactive"})
	writeFile(t, filepath.Join(claudeDir, "sessions", fmt.Sprintf("%d.json", pid)), string(entry), time.Time{})
	writeFile(t, filepath.Join(claudeDir, "projects", "-repo", id+".jsonl"),
		`{"type":"user","cwd":"/repo","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n", now.Add(-age))
}

// A live pid beats a stale transcript under --running; a dead pid and an unparsable file add nothing.
func TestRegistryJoin(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	stubPidAlive(t, func(pid int) bool { return pid == os.Getpid() })

	regFixture(t, claudeDir, "live-stale", os.Getpid(), 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-stale", deadPID, 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-fresh", deadPID-1, time.Minute, now)
	writeFile(t, filepath.Join(claudeDir, "sessions", "junk.json"), "{not json", time.Time{})
	writeFile(t, filepath.Join(claudeDir, "sessions", "noid.json"), `{"pid":1,"name":"x"}`, time.Time{})

	tree, err := build(claudeDir, Options{Days: 1, Running: true, Now: pinNow(now)})
	if err != nil {
		t.Fatalf("build with an unparsable registry file: %v", err)
	}
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
	if !got.Live || got.PID != os.Getpid() || got.Name != "name-live-stale" || got.CLIStatus != "waiting" || got.Mode != "interactive" {
		t.Errorf("registry fields not joined: %+v", got)
	}
}

// A live pid without a sessionId and an unparsable file yield no registry entry.
func TestReadRegistrySkipsUnusableEntries(t *testing.T) {
	claudeDir := t.TempDir()
	stubPidAlive(t, func(int) bool { return true })
	for name, body := range map[string]string{"junk.json": "{not json", "noid.json": `{"pid":7,"name":"x"}`, "ok.json": `{"pid":8,"sessionId":"s8"}`} {
		writeFile(t, filepath.Join(claudeDir, "sessions", name), body, time.Time{})
	}
	if got := readRegistry(claudeDir); len(got) != 1 || got["s8"].PID != 8 {
		t.Errorf("readRegistry = %+v, want only s8", got)
	}
}
