package agenttree

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// New coverage for the journal-backed status, activity, vendor-legs and drill-down work
// (plans/2026-10-18-agents-tree-visibility.md Part 1). Written after `go build ./internal/agenttree`
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
	if gate.Status != statusFinished {
		t.Errorf("fakewfagent002 status = %q, want finished (journal result line, meta has no finished:true)", gate.Status)
	}

	doomed, ok := subagentByID(s, "fakewfagent030")
	if !ok {
		t.Fatal("fakewfagent030 not found")
	}
	if doomed.Status != statusError {
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
	if busy.Status != statusRunning {
		t.Errorf("fakewfagent020 status = %q, want running (started, no result, fresh mtime)", busy.Status)
	}
	run := workflowByID(t, s.Workflows, "wf_fake0003-running")
	if run.Status != statusRunning {
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
	if run.Status != statusFinished {
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

// (6) A relay subagent's Legs carry every vendor dispatch its transcript's exit lines named: plain
// ok, still-running, and a fallback (Fallback+FallbackReason set, matched=="" since read directly).
func TestVendorLegsDirectMatch(t *testing.T) {
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
	if len(gate.Legs) != 3 {
		t.Fatalf("Legs = %+v, want 3 (codex ok, gemini running, codex->claude fallback)", gate.Legs)
	}
	byID := map[string]Leg{}
	for _, l := range gate.Legs {
		byID[l.ID] = l
	}
	if l := byID["20261017T231131.948-codex"]; l.Status != "ok" || l.Matched != "" {
		t.Errorf("codex leg = %+v, want status=ok matched=\"\"", l)
	}
	if l := byID["20261018T095401.709-gemini"]; l.Status != "running" || l.Duration == "" {
		t.Errorf("gemini leg = %+v, want status=running with an elapsed duration", l)
	}
	fb := byID["20261018T150000.000-codex"]
	if fb.Fallback != "claude" || fb.FallbackReason == "" {
		t.Errorf("fallback leg = %+v, want Fallback=claude with a reason", fb)
	}
	if !strings.Contains(formatLeg(fb), "⚠ fallback") {
		t.Errorf("formatLeg(fallback) = %q, want it to flag the fallback", formatLeg(fb))
	}
}

// (7) A relay whose own transcript hasn't logged its `ask` exit line yet (still mid-dispatch) is
// matched heuristically against the dispatch log's own still-running records: same WorkDir, started
// no earlier than the subagent itself.
func TestVendorLegHeuristicRunningMatch(t *testing.T) {
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
	var heuristic *Leg
	for i := range busy.Legs {
		if busy.Legs[i].Matched == "heuristic" {
			heuristic = &busy.Legs[i]
		}
	}
	if heuristic == nil {
		t.Fatalf("Legs = %+v, want one heuristically matched leg", busy.Legs)
	}
	if heuristic.ID != "20261018T100500.000-gemini" {
		t.Errorf("heuristic leg id = %q, want the still-running gemini dispatch", heuristic.ID)
	}
}

// (8) View's Snapshot carries the edited files, legs and a result summary for a finished workflow
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
	if snap.Status != statusFinished {
		t.Errorf("snapshot status = %q, want finished", snap.Status)
	}
	wantEdited := []string{"/Users/fake/demo-repo/internal/agenttree/scan.go"}
	if len(snap.FilesEdited) != 1 || snap.FilesEdited[0] != wantEdited[0] {
		t.Errorf("FilesEdited = %v, want %v", snap.FilesEdited, wantEdited)
	}
	if len(snap.Legs) != 3 {
		t.Errorf("Legs = %+v, want 3", snap.Legs)
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
		done <- Follow(context.Background(), ViewOptions{ClaudeDir: claudeDir, Session: "finished-session-aaaa1111", Agent: "fakewfagent002", Now: pinNow(now)}, &buf)
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

// Regression: the pre-change JSON schema (lastTool, no activity/legs) still decodes a tree built
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
