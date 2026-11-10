package tree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/transcript"
)

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

// (5) transcript.DispatchIDs greps every "ask: dispatch <id>" exit line out of a relay transcript, deduped, in
// the order they first appear.
func TestDispatchIDsExtraction(t *testing.T) {
	path := filepath.Join("testdata", "projects", "-Users-fake-demo-repo", "finished-session-aaaa1111",
		"subagents", "workflows", "wf_fake0001-abc", "agent-fakewfagent002.jsonl")
	ids := transcript.DispatchIDs(path)
	want := []string{"20261017T231131.948-codex", "20261018T095401.709-gemini", "20261018T150000.000-codex"}
	if len(ids) != len(want) {
		t.Fatalf("DispatchIDs = %v, want %v", ids, want)
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

// A dispatch id printed only as the envelope "dispatch_id" (no `ask: dispatch` line) still yields its Dispatch.
func TestBuildDispatchesEnvelopeID(t *testing.T) {
	withDispatchFixture(t)
	path := filepath.Join(t.TempDir(), "agent.jsonl")
	line := `{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"dispatch_id\":\"20261017T231131.948-codex\"}"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := buildDispatches(path, "", time.Now(), Activity{})
	if len(got) != 1 || got[0].ID != "20261017T231131.948-codex" {
		t.Fatalf("buildDispatches = %+v, want the envelope dispatch", got)
	}
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
