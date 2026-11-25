package tree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude" // the vendors register their roster rows from their own init
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/worktree"
)

const (
	finishedSID = "finished-session-aaaa1111"
	staleSID    = "stale-session-cccc3333"
	runningSID  = "running-session-bbbb2222"
	demoProj    = "-Users-fake-demo-repo"
	jobSID      = "memory-job-eeee5555"
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

func subagentsByID(s Session) map[string]Subagent {
	byID := map[string]Subagent{}
	for _, sub := range s.Subagents {
		byID[sub.ID] = sub
	}
	return byID
}

// TestBuildFixtureSession reads the finished session once; its subtests share that one tree.
func TestBuildFixtureSession(t *testing.T) {
	t.Parallel()
	tree := buildShared(t, Options{Days: 60})
	s := sessionByID(t, tree, finishedSID)

	t.Run("subagents by depth", func(t *testing.T) {
		t.Parallel()
		byID := subagentsByID(s)
		if len(s.Subagents) != 2 {
			t.Fatalf("got %d subagents, want 2: %+v", len(s.Subagents), s.Subagents)
		}
		if byID["fakeagentid001"].Depth != 1 || byID["fakeagentid002"].Depth != 2 || byID["fakeagentid001"].Type != "copilot" {
			t.Errorf("agent001 = %+v, agent002 = %+v, want depth 1/2 and type copilot", byID["fakeagentid001"], byID["fakeagentid002"])
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
}

// TestBuildFixtureStatus reads the same finished session's statuses, tokens and journal-driven runs.
func TestBuildFixtureStatus(t *testing.T) {
	t.Parallel()
	tree := buildShared(t, Options{Days: 60})
	s := sessionByID(t, tree, finishedSID)
	t.Run("status uses the pinned clock", func(t *testing.T) {
		t.Parallel()
		if got := sessionByID(t, tree, runningSID).Status; got != statusActive {
			t.Errorf("running-session status = %q, want active", got)
		}
		if got := sessionByID(t, tree, staleSID).Status; got != statusIdle {
			t.Errorf("stale-session status = %q, want idle", got)
		}
		byID := subagentsByID(s)
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
		a := subagentsByID(s)["fakeagentid001"]
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
}

func TestFilterIdleSessionsKeepsOnlyActiveWork(t *testing.T) {
	t.Parallel()
	tree := buildShared(t, Options{Days: 60})
	kept := filterIdleSessions(tree.Sessions)
	for _, s := range kept {
		if !s.isActive() {
			t.Errorf("inactive session retained: %s", s.ID)
		}
	}
	for _, s := range tree.Sessions {
		if s.isActive() && !slices.ContainsFunc(kept, func(k Session) bool { return k.ID == s.ID }) {
			t.Errorf("active session dropped: %s", s.ID)
		}
	}
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
		{"sdk-cli harness job is not a top-level session", Options{Days: 1}, []string{finishedSID}, []string{jobSID}, nil},
		{"session narrows to one id ignoring days", Options{Session: staleSID}, nil, nil, []string{staleSID}},
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

// An unfinished subagent past activeWindow is stale: it may be hung, not done.
func TestSubagentGoesStaleOutsideActiveWindow(t *testing.T) {
	t.Parallel()
	claudeDir, now := copyTestdataTree(t)
	touch(t, agent002Path(filepath.Join(claudeDir, "projects", demoProj)), now.Add(-time.Hour))
	tree, err := build(claudeDir, Options{Days: 60, Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustSub(t, sessionByID(t, tree, finishedSID), "fakeagentid002").Status; got != statusStale {
		t.Errorf("agent002 status = %q, want stale", got)
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
			tree, err := listing(e.cfg, Options{Days: 60, Now: pinNow(e.now)}, true)
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

// A live pid beats a stale transcript; a dead pid and an unparsable file add nothing.
func TestRegistryJoin(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	stubPidAlive(t, func(pid int) bool { return pid == os.Getpid() })

	regFixture(t, claudeDir, "live-stale", os.Getpid(), 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-stale", deadPID, 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-fresh", deadPID-1, time.Minute, now)
	writeFile(t, filepath.Join(claudeDir, "sessions", "junk.json"), "{not json", time.Time{})
	writeFile(t, filepath.Join(claudeDir, "sessions", "noid.json"), `{"pid":1,"name":"x"}`, time.Time{})

	tree, err := build(claudeDir, Options{Days: 1, Now: pinNow(now)})
	if err != nil {
		t.Fatalf("build with an unparsable registry file: %v", err)
	}
	var got *Session
	for i, s := range tree.Sessions {
		switch s.ID {
		case "dead-stale":
			t.Errorf("dead pid with stale transcript listed")
		case "dead-fresh":
			if s.Live || s.PID != 0 {
				t.Errorf("dead pid marked live: %+v", s)
			}
		case "live-stale":
			got = &tree.Sessions[i]
		}
	}
	if got == nil {
		t.Fatal("live pid with a 40-day-old transcript dropped")
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

func TestActiveSessionKeepsRunningAncestry(t *testing.T) {
	s := Session{Status: statusActive, Subagents: []Subagent{
		{ID: "idle", Status: statusStale},
		{ID: "parent", Status: statusFinished, Dispatches: []Dispatch{
			{ID: "root", Status: "ok"},
			{ID: "child", Parent: "root", Status: statusRunning},
			{ID: "done", Status: "ok"},
		}},
	}, Workflows: []WorkflowRun{{Status: statusFinished}}}
	got := activeSession(s)
	if len(got.Subagents) != 1 || got.Subagents[0].ID != "parent" || len(got.Subagents[0].Dispatches) != 2 || len(got.Workflows) != 0 {
		t.Fatalf("active session = %+v", got)
	}
	if len(s.Subagents) != 2 || len(s.Subagents[1].Dispatches) != 3 || len(s.Workflows) != 1 {
		t.Fatal("filter changed the complete source tree")
	}
}

func TestAttachHoldingsShowsHeldFilesNoteAndFreeOnlyWhenAsked(t *testing.T) {
	hosttest.SkipOnWindows(t, "git repo and pid liveness")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := hosttest.Repo(t)
	hosttest.WriteRel(t, repo, "held.go", "package x\n")
	hosttest.WriteRel(t, repo, "loose.go", "package x\n")
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(worktree.Claim{Path: filepath.Join(repo, "held.go"), Agent: "ag1", TS: time.Now().Unix(), OwnerStart: start})
	ledger := filepath.Join(worktree.ClaimsDir(), strconv.Itoa(os.Getpid())+"-sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil || os.WriteFile(ledger, append(line, '\n'), 0o600) != nil {
		t.Fatal("write ledger", err)
	}
	claimLiveLeaf(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess1")
	t.Setenv(proc.DispatchIDEnv, "")
	if sent, err := worktree.NoteDispatcher("half done"); !sent || err != nil {
		t.Fatalf("note: %v %v", sent, err)
	}
	mk := func() *Tree {
		return &Tree{Sessions: []Session{{ID: "sess1", Subagents: []Subagent{{ID: "ag1", Type: "claude", Status: "running"}}}}}
	}

	plain := mk()
	var buf bytes.Buffer
	renderTree(&buf, plain, time.Now(), 0, "")
	if strings.Contains(buf.String(), "held.go") || strings.Contains(buf.String(), "free:") {
		t.Errorf("default view changed:\n%s", buf.String())
	}

	withFiles := mk()
	attachHoldings(withFiles, repo, time.Now())
	buf.Reset()
	renderTree(&buf, withFiles, time.Now(), 0, "")
	for _, want := range []string{"files: held.go", "note: half done", "free: 1 dirty", "loose.go"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--files view lacks %q:\n%s", want, buf.String())
		}
	}
}

// writeBenchRelayTranscript writes a relay subagent transcript + meta: one Bash ask and tool_result per dispatch id.
func writeBenchRelayTranscript(b *testing.B, path string, ids []string) {
	b.Helper()
	var sb strings.Builder
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for i, id := range ids {
		ts := at.Add(time.Duration(i) * 30 * time.Second).Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"type":"assistant","timestamp":%q,"message":{"id":"msg_%04d","model":"claude-sonnet-4-5",`+
			`"content":[{"type":"tool_use","name":"Bash","input":{"command":"claude-code ask --agent codex \"leaf %d\""}}],`+
			`"usage":{"input_tokens":1400,"cache_read_input_tokens":320,"output_tokens":48}}}`+"\n", ts, i, i)
		fmt.Fprintf(&sb, `{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"toolu_%04d",`+
			`"content":"ask: dispatch %s\nOK"}]}}`+"\n", ts, i, id)
	}
	// the tail activity must not be a pending ask, or every call also scans RecentRecords(50)
	fmt.Fprintf(&sb, `{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"tool_use","name":"Read",`+
		`"input":{"file_path":"/repo/internal/agent/tree/reap.go"}}]}}`+"\n",
		at.Add(time.Duration(len(ids))*30*time.Second).Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	meta := `{"agentType":"general-purpose","description":"relay leaf: fan out asks","finished":true,"spawnDepth":1}`
	if err := os.WriteFile(metaPath(path), []byte(meta), 0o600); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkBuildSubagent50FinishedDispatches re-reads 50 finished dispatch records for a scan-cached relay subagent.
func BenchmarkBuildSubagent50FinishedDispatches(b *testing.B) {
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	ids := writeBenchDispatchLog(b, 50, 0)
	path := filepath.Join(b.TempDir(), "agent-benchrelay01.jsonl")
	writeBenchRelayTranscript(b, path, ids)
	scans := loadScanCache()
	now, cwd := time.Now(), "/repo"
	if sub := buildSubagent(path, now, cwd, scans); len(sub.Dispatches) != len(ids) { // warm the scan/meta/activity cache
		b.Fatalf("warm-up: got %d dispatches, want %d", len(sub.Dispatches), len(ids))
	}
	b.ReportAllocs()
	for b.Loop() {
		if sub := buildSubagent(path, now, cwd, scans); len(sub.Dispatches) != len(ids) {
			b.Fatalf("got %d dispatches, want %d", len(sub.Dispatches), len(ids))
		}
	}
}

// claimLiveLeaf gives agent ag1 of session sess1 a live in-flight marker owned by this process.
func claimLiveLeaf(t *testing.T) {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "sess1.jsonl")
	sub := inflight.SubagentDir(transcript)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"agent-ag1.meta.json": `{"toolUseId":"toolu_x"}`, "agent-ag1.jsonl": "{}\n"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := inflight.TryClaim(inflight.Claim{Owner: os.Getpid(), ID: "toolu_x", Transcript: transcript}, inflight.Limits{Total: 9}); !ok {
		t.Fatal("marker not claimed")
	}
}

func treeWidthNodes() []Node {
	long := strings.Repeat("界e\u0301", 80)
	model := "fixture-model-5-" + strings.Repeat("long-", 40)
	job := Dispatch{ID: "fake-job-" + long, Agent: "fake-agent-" + long, Model: model,
		Provider: "fixture", Status: statusRunning, Duration: strings.Repeat("fixture-duration-", 10)}
	root, queued := job, job
	root.ID, queued.ID = "fake-root-"+long, "fake-queued-"+long
	queued.Status, queued.queued = "queued", "waiting for fixture slot"
	session := Session{ID: "fake-session-" + long, Name: long, Title: long, Branch: long, Status: statusActive,
		Workflows: []WorkflowRun{{ID: "fake-workflow-" + long, Name: long, Status: statusRunning}},
		Subagents: []Subagent{{ID: "fake-subagent-" + long, Type: long, Status: statusRunning,
			Description: model + ": " + long, Dispatches: []Dispatch{job}}}}
	return Build([]Session{session}, []Dispatch{root, queued})
}

func TestTreeRowWidths(t *testing.T) {
	nodes := treeWidthNodes()
	open := map[string]bool{}
	for _, n := range visible(nodes, nil) {
		open[nodeKey(n)] = true
	}
	rows := visible(nodes, open)
	for _, tt := range []struct {
		name string
		id   string
	}{{"session", nodes[0].ID}, {"workflow run", nodes[0].Children[1].ID}, {"subagent", nodes[0].Children[0].ID},
		{"session dispatch", nodes[0].Children[0].Children[0].ID}, {"root dispatch", nodes[1].ID},
		{"queued dispatch", nodes[2].ID}} {
		row := slices.IndexFunc(rows, func(n Node) bool { return n.ID == tt.id })
		t.Run(tt.name, func(t *testing.T) {
			for _, width := range []int{120, 80, 60, 40, 20} {
				t.Run(strconv.Itoa(width), func(t *testing.T) {
					s := clamp(dashState{width: width, height: 24, open: open, selID: nodeKey(rows[row])}, rows)
					lines := dashLines(s, nodes, time.Unix(0, 0), "\x1b[36m")
					if len(lines) != len(rows)+footerRows {
						t.Fatalf("got %d lines, want %d", len(lines), len(rows)+footerRows)
					}
					for _, line := range lines {
						if got := textutil.DisplayWidth(ansi.Strip(line)); got > min(width, 80) {
							t.Errorf("line width %d exceeds %d: %q", got, min(width, 80), line)
						}
					}
					if line := printable(ansi.Strip(lines[row])); !strings.Contains(line, "…") {
						t.Errorf("over-long row lacks final ellipsis: %q", line)
					}
				})
			}
		})
	}
}

func TestTreeFoldDepths(t *testing.T) {
	var records []agent.Record
	parent := ""
	for i := range 5 {
		carrier := modelRecord(fmt.Sprintf("fake-carrier-%d", i), parent, "codex", true)
		worker := modelRecord(fmt.Sprintf("fake-worker-%d", i), carrier.ID, "pi", false)
		records = append(records, carrier, worker)
		parent = worker.ID
	}
	for _, depth := range []int{1, 2, 3, 4, 5} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			fixture := records[:2*depth]
			nodes := Build(nil, modelDispatches(fixture...))
			for i := range depth {
				if len(nodes) != 1 {
					t.Fatalf("level %d: got %d nodes, want one", i, len(nodes))
				}
				n := nodes[0]
				carrier, worker := fixture[2*i], fixture[2*i+1]
				if n.Depth != i || n.ID != worker.ID || n.Via != carrier.ID {
					t.Fatalf("level %d: got depth=%d id=%q via=%q, want %d %q %q",
						i, n.Depth, n.ID, n.Via, i, worker.ID, carrier.ID)
				}
				assertTreeWorkerID(t, n.ID, fixture)
				nodes = n.Children
			}
			if len(nodes) != 0 {
				t.Fatalf("extra nodes below depth %d: %+v", depth, nodes)
			}
		})
	}
}

func assertTreeWorkerID(t *testing.T, id string, records []agent.Record) {
	t.Helper()
	for _, rec := range records {
		if rec.Request.Carry && id == rec.ID {
			t.Errorf("carrier %q appeared as a node ID", rec.ID)
		}
	}
}

func TestTreeKeyScripts(t *testing.T) {
	nodes := rootNodes(40)
	for i := range nodes {
		nodes[i].Dispatch = &Dispatch{ID: nodes[i].ID, Agent: "fixture", Status: statusRunning}
	}
	selected := len(nodes) / 2
	for _, tt := range []struct {
		name, raw string
		cancels   int
		step      int
	}{
		{"unbound n", "cnY", 0, 0}, {"unbound x", "cxY", 0, 0}, {"unbound digit", "c1Y", 0, 0},
		{"unbound space", "c Y", 0, 0}, {"delete", "c\x1b[3~Y", 0, 0}, {"shift tab", "c\x1b[ZY", 0, 0},
		{"down disarms", "c\x1b[BY", 0, 0}, {"up disarms", "c\x1b[AY", 0, 0},
		{"down after disarming", "c\x1b[B\x1b[BY", 0, 1}, {"up after disarming", "c\x1b[A\x1b[AY", 0, -1},
		{"confirm once", "cYY", 1, 0}, {"lowercase y", "cyY", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := clamp(dashState{width: 80, height: 10, selID: nodeKey(nodes[selected])}, visible(nodes, nil))
			s, effects := runTreeKeyScript(t, s, nodes, tt.raw)
			var cancelled []string
			for _, e := range effects {
				if e.Kind == effectCancel {
					cancelled = append(cancelled, e.ID)
				}
			}
			want := make([]string, tt.cancels)
			for i := range want {
				want[i] = nodes[selected].Dispatch.ID
			}
			if !slices.Equal(cancelled, want) {
				t.Errorf("cancel effects = %v, want %v", cancelled, want)
			}
			if s.pending != "" || s.selID != nodeKey(nodes[selected+tt.step]) {
				t.Errorf("final pending=%q selection=%q, want empty and %q", s.pending, s.selID, nodeKey(nodes[selected+tt.step]))
			}
			if last := effects[len(effects)-1]; last.Kind != effectNone {
				t.Errorf("Y after disarming produced %+v", last)
			}
		})
	}
}

func runTreeKeyScript(t *testing.T, s dashState, nodes []Node, raw string) (dashState, []effect) {
	t.Helper()
	var effects []effect
	for i, k := range decodeKeys([]byte(raw)) {
		var e effect
		s, e = onKey(s, k, nodes)
		effects = append(effects, e)
		if i == 0 && s.pending != nodes[s.sel].Dispatch.ID {
			t.Fatalf("cancel did not arm selected job: pending=%q", s.pending)
		}
		if i > 0 && s.pending != "" {
			t.Fatalf("key %d did not disarm cancel: pending=%q", i, s.pending)
		}
		if e.Kind == effectCopy {
			t.Fatalf("script %q copied job %q", raw, e.ID)
		}
	}
	return s, effects
}

func TestRunningAgentIsTheModelThatRan(t *testing.T) {
	for model, want := range map[string]string{"databricks-gpt-6-1-sol": "codex", "databricks-kimi-k3": "kimi", "": "pi", "auto": "pi"} {
		sub := Subagent{Type: "claude", Description: "Dispatching to pi", Dispatches: []Dispatch{{ID: "p", Agent: "pi", Model: model, Status: "ok"}}}
		if got := runningAgent(sub); got != want {
			t.Errorf("model %q: agent = %q, want %q", model, got, want)
		}
	}
}

func TestCodeModeDispatchShowsCoworkerAndKeepsItsModelColumn(t *testing.T) {
	d := Dispatch{ID: "p", Agent: "pi", Model: "databricks-gpt-6-1-sol", Status: "ok", codeMode: true}
	if got := runningAgent(Subagent{Type: "claude", Description: "to pi", Dispatches: []Dispatch{d}}); got != "coworker" {
		t.Errorf("codemode type = %q, want coworker", got)
	}
	d.codeMode = false
	if _, ok := ownRecord(Subagent{Type: "codex", Dispatches: []Dispatch{d}}); !ok {
		t.Error("a pi run on a GPT id lost its record for the codex row")
	}
}

func TestRelayKeepsItsOwnDispatchWhenClaudeAnswered(t *testing.T) {
	d := Dispatch{ID: "p", Agent: "pi", Model: "claude-opus-5-5", Status: "ok"}
	sub := Subagent{Type: "claude", Description: "to pi", Dispatches: []Dispatch{{ID: "c", Agent: "codex", Model: "gpt-5-4-mini"}, d}}
	sub.own = "p"
	if got, ok := ownRecord(sub); !ok || got.ID != "p" {
		t.Errorf("ownRecord = %q, %v, want the pi dispatch", got.ID, ok)
	}
	opts := agent.RunOptions{Codemode: "on"}
	if answeredInCodeMode(agent.Record{Agent: "pi", RanAs: "codex", Request: opts}) {
		t.Error("a fallback answer still counts as codemode")
	}
	if !answeredInCodeMode(agent.Record{Agent: "pi", Fallback: "pi", Request: opts}) {
		t.Error("a same-pi route fallback lost its codemode")
	}
}
