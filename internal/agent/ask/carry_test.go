package ask

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/contract"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/route"
)

// Only an external agent whose row sets Carry is carried: pi, claude and a persona are refused.
func TestCarryAgentRefusals(t *testing.T) {
	for _, c := range []struct {
		name    string
		persona bool
		want    string
	}{
		{"pi", false, "not carried"}, {"claude", false, "not carried"}, {"gemini", true, "not carried"},
		{"codex", false, ""}, {"gemini", false, ""},
	} {
		err := carryAgent(c.name, c.persona)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("carryAgent(%s, persona %v) = %v, want %q", c.name, c.persona, err, c.want)
		}
	}
}

// A cut-off child goes to Pi; an ok, failed or missing child does not; --fallback none with a typed model refuses.
func TestTakeoverDecision(t *testing.T) {
	cut := agent.Record{ID: "c", Status: "stalled", Cutoff: agent.CutoffDeadline}
	for _, c := range []struct {
		child          agent.Record
		found, typed   bool
		policy         string
		take, explains bool
	}{
		{cut, true, false, route.FallbackNone, true, false},
		{cut, true, true, "all", true, false},
		{cut, true, true, route.FallbackNone, false, true},
		{agent.Record{Status: "ok"}, true, false, "all", false, false},
		{agent.Record{Status: "error"}, true, false, "all", false, false},
		{cut, false, false, "all", false, false},
	} {
		take, why := takeoverDecision(c.child, c.found, c.policy, c.typed)
		if take != c.take || (why != "") != c.explains {
			t.Errorf("%+v: take %v why %q, want take %v explains %v", c, take, why, c.take, c.explains)
		}
	}
}

// A cut-off carried child is finished by Pi under the inner line's tools, given the task and partial output.
func TestRunCarryTakesOverCutOffChild(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const outer, child, piID = "20261122T010000.000-gemini", "20261122T010000.001-gemini", "20261122T010000.002-pi"
	inner := []string{"claude-code", "agents", "ask", "--agent", "gemini", "--tools", "-"}
	rec := agent.Record{ID: outer, Agent: "gemini", Status: "running", FallbackPolicy: "all",
		Request: agent.RunOptions{Prompt: "the task", Carry: true, CarryArgv: inner}}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	old := carryExec
	t.Cleanup(func() { carryExec = old })
	carryExec = func(_ context.Context, argv []string, stdin io.Reader, _ []string, stdout, _ io.Writer) int {
		if calls++; calls == 1 {
			_, _ = io.WriteString(stdout, "partial words")
			return saveChild(t, agent.Record{ID: child, Agent: "gemini", Parent: outer, Status: "stalled", Cutoff: agent.CutoffDeadline}, 1)
		}
		checkTakeover(t, argv, stdin)
		return saveChild(t, agent.Record{ID: piID, Agent: "pi", Parent: outer, Status: "ok", Model: "m"}, 0)
	}
	if code := runCarry(t.Context(), rec, io.Discard); code != 0 || calls != 2 {
		t.Fatalf("runCarry = %d after %d calls, want 0 after 2", code, calls)
	}
	checkOuter(t, outer, child, piID)
}

// A background carrier sends the answer and the ask trailer down one stream, so the trailer cannot land mid-answer.
// A foreground carrier keeps the trailer on stderr and tees only the answer into the record log.
func TestCarryLogJoinsStreamsOnlyInBackground(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bg := agent.Record{ID: "b", Background: true}
	if err := os.MkdirAll(filepath.Dir(bg.LogPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	out, errOut, _, closeLog := carryLog(bg, io.Discard)
	closeLog()
	if out != errOut {
		t.Fatal("background carry: stdout and stderr are different writers")
	}
	var answer bytes.Buffer
	fg := agent.Record{ID: "20261122T030000.000-gemini", Agent: "gemini"}
	out, errOut, _, closeLog = carryLog(fg, &answer)
	t.Cleanup(closeLog)
	io.WriteString(out, "the answer\n")
	if errOut != os.Stderr {
		t.Fatal("foreground carry: stderr left os.Stderr")
	}
	if answer.String() != "the answer\n" {
		t.Fatalf("foreground stdout = %q, want the answer only", answer.String())
	}
	log, err := os.ReadFile(fg.LogPath())
	if err != nil || string(log) != "the answer\n" {
		t.Fatalf("foreground log = %q (%v), want the answer only", log, err)
	}
}

func TestLockedWriterEventNewlineGuard(t *testing.T) {
	for _, answer := range []string{"", "answer", "answer\n"} {
		var out bytes.Buffer
		locked := &lockedWriter{w: &out}
		if _, err := io.WriteString(locked, answer); err != nil {
			t.Fatal(err)
		}
		locked.event("tool_use", "via child: Read")
		locked.event("tool_result", "via child: ok")
		want := answer
		if answer == "answer" {
			want += "\n"
		}
		want += "▸ tool_use: via child: Read\n▸ tool_result: via child: ok\n"
		if out.String() != want {
			t.Fatalf("answer %q: log = %q, want %q", answer, out.String(), want)
		}
	}
}

// A takeover brief tells Pi to read every file it needs and to batch the independent reads into one response.
func TestTakeoverBriefAsksForOneReadBatch(t *testing.T) {
	got := takeoverBrief("codex", "deadline", "the task", 4, "tail")
	if !strings.Contains(got, "batch all of those independent reads into one response") {
		t.Fatalf("takeover brief = %q, want the one-batch read instruction", got)
	}
	if !strings.Contains(got, "the task") || !strings.Contains(got, "tail") {
		t.Fatalf("takeover brief = %q, want the task and the partial output kept", got)
	}
}

// checkOuter asserts the outer record ran as pi, holds pi's result event under its own id, and pi links the child.
func checkOuter(t *testing.T, outer, child, piID string) {
	t.Helper()
	got, _ := agent.LoadRecord(outer)
	pi, _ := agent.LoadRecord(piID)
	if got.Status != "ok" || got.RanAs != "pi" || pi.TakeoverOf != child {
		t.Errorf("outer %s ran_as %q, pi takeover_of %q: want ok, pi, %s", got.Status, got.RanAs, pi.TakeoverOf, child)
	}
	if env, ok := got.SavedEnvelope(); !ok || env.DispatchID != outer || env.Agent != "pi" {
		t.Errorf("outer result event %+v (found %v): want pi's envelope under %s", env, ok, outer)
	}
}

// A toolless carried run gets a toolless Pi.
func TestCarryLinesKeepToolsTier(t *testing.T) {
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--agent", "gemini", "--tools=false"}); err != nil {
		t.Fatal(err)
	}
	inner := canonicalArgv(fs, f.listValues(), "task", true, nil)
	pi, err := takeoverArgv(t.Context(), inner)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pi, "--tools") || !slices.Contains(pi, "--tools=false") {
		t.Errorf("toolless carried run: pi line %q grants tools", pi)
	}
}

// checkTakeover asserts Pi's line keeps --tools, and its brief has the task and partial output.
func checkTakeover(t *testing.T, argv []string, stdin io.Reader) {
	t.Helper()
	in, _ := io.ReadAll(stdin)
	if !slices.Contains(argv, "pi") || !slices.Contains(argv, "--tools") {
		t.Errorf("takeover argv %q: want --agent pi --tools", argv)
	}
	if !strings.Contains(string(in), "the task") || !strings.Contains(string(in), "partial words") {
		t.Errorf("takeover brief %q lacks the task or the partial output", in)
	}
}

func saveChild(t *testing.T, r agent.Record, code int) int {
	t.Helper()
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	return code
}

// A foreground stream-json carrier stamps the child's events with via and ends on its own single result.
func TestRunCarryStreamJSONStampsVia(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const outer, child = "20261122T020000.000-gemini", "20261122T020000.001-gemini"
	inner := []string{"claude-code", "agents", "ask", "--agent", "gemini", "--format", "stream-json", "-"}
	rec := agent.Record{ID: outer, Agent: "gemini", Status: "running",
		Request: agent.RunOptions{Prompt: "the task", Carry: true, CarryArgv: inner}}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	old := carryExec
	t.Cleanup(func() { carryExec = old })
	carryExec = func(_ context.Context, _ []string, _ io.Reader, _ []string, stdout, _ io.Writer) int {
		lines := `{"dispatch_id":"` + child + `","type":"text","text":"hi"}` + "\n" +
			`{"dispatch_id":"` + child + `","type":"result","status":"ok"}` + "\n"
		_, _ = io.WriteString(stdout, lines[:20]) // a write may end mid-line
		_, _ = io.WriteString(stdout, lines[20:])
		return saveChild(t, agent.Record{ID: child, Agent: "gemini", Parent: outer, Status: "ok"}, 0)
	}
	var out bytes.Buffer
	if code := runCarry(t.Context(), rec, &out); code != 0 {
		t.Fatalf("runCarry = %d, want 0", code)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(got) != 2 || !strings.Contains(got[0], `"via":"`+child+`"`) || !strings.Contains(got[1], `"type":"result"`) ||
		!strings.Contains(got[1], `"dispatch_id":"`+outer+`"`) || strings.Contains(got[1], `"via"`) {
		t.Fatalf("stream = %q, want a via-stamped text line then the carrier's one result", got)
	}
}

func TestTakeCarryVerifyLeavesEnvAndContext(t *testing.T) {
	t.Setenv(carryVerifyEnv, "go vet ./x")
	ctx := proc.WithValues(context.Background(), proc.Values{carryVerifyEnv: "go vet ./x", "A": "1"})
	ctx, got := takeCarryVerify(ctx)
	if v, set := os.LookupEnv(carryVerifyEnv); got != "go vet ./x" || set {
		t.Errorf("take = %q, env still set = %v (%q)", got, set, v)
	}
	if values, _ := proc.ValuesFromContext(ctx); values[carryVerifyEnv] != "" || values["A"] != "1" {
		t.Errorf("ctx values = %v", values)
	}
}

func TestTakeCarryVerifyReadsTheSnapshotWhenOneIsInstalled(t *testing.T) {
	t.Setenv(carryVerifyEnv, "ambient")
	_, got := takeCarryVerify(proc.WithValues(context.Background(), proc.Values{carryVerifyEnv: "snapshot"}))
	if got != "snapshot" {
		t.Errorf("take = %q, want the snapshot's value", got)
	}
}

func TestFencedWorkspaceIsTheConfinementOfAProviderOnlyAsk(t *testing.T) {
	ctx := proc.WithValues(context.Background(), proc.Values{"COWORKER_OVERLAY": `{"provider":"databricks","profile":"prod"}`})
	if got := fencedWorkspace(ctx, &askFlags{provider: "databricks"}); got != "prod" {
		t.Errorf("fenced = %q, want prod", got)
	}
	if got := fencedWorkspace(ctx, &askFlags{provider: "databricks", profile: "dev"}); got != "" {
		t.Errorf("a typed profile keeps its own lead, got %q", got)
	}
}

func TestTakeoverDecisionSkipsBudgetEnd(t *testing.T) {
	child := agent.Record{ID: "c", Status: "stalled", Cutoff: agent.CutoffIdle, BudgetKind: "idle"}
	take, why := takeoverDecision(child, true, "all", false)
	if take || why == "" {
		t.Fatalf("a budget end must not be taken over by Pi: take=%v why=%q", take, why)
	}
	child.BudgetKind = ""
	if take, _ := takeoverDecision(child, true, "all", false); !take {
		t.Fatal("a stall before any output keeps its Pi takeover")
	}
}

func TestContractBudgetMarksLimitAndDeadline(t *testing.T) {
	run := errors.New("cut")
	for _, why := range []contract.Reason{contract.ReasonLimit, contract.ReasonDeadline} {
		if got := contractBudget(contract.Contract{Failed: why}, run); retry.BudgetOf(got) != retry.BudgetContract {
			t.Errorf("%s: budget %q", why, retry.BudgetOf(got))
		}
	}
	if got := contractBudget(contract.Contract{Failed: contract.ReasonOwnerDead}, run); retry.IsBudget(got) {
		t.Error("owner-dead is not a budget end")
	}
}

// An answered run whose contract then fails on its limit ends as a contract budget failure that keeps the answer.
func TestContractBudgetAfterAnswerKeepsResult(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := contractBudget(contract.Contract{ID: "k", Failed: contract.ReasonLimit}, nil)
	if retry.BudgetOf(err) != retry.BudgetContract || !answeredBeforeContractEnd(err) {
		t.Fatalf("answered run lost its contract end: %v", err)
	}
	rec := agent.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	res := wire.Result{Agent: "claude", Answer: "done"}
	_ = rec.Conclude(res, err, agent.RunRecordedOpts{}) // returns err itself; the saved record is what counts
	saved, lerr := agent.LoadRecord(rec.ID)
	env := agent.EnvelopeOf(res, err, rec.ID)
	if lerr != nil || saved.Status != "error" || saved.BudgetKind != "contract" || !strings.HasPrefix(agent.ResumeHint(saved), "Budget end:") {
		t.Fatalf("record: %v %+v", lerr, saved)
	}
	if env.Answer != "done" || env.BudgetKind != "contract" || env.Status != wire.StatusError {
		t.Fatalf("envelope: %+v", env)
	}
}

func TestMoveOffCutoffSkipsBudgetRecord(t *testing.T) {
	rec := agent.Record{ID: "d", Cutoff: agent.CutoffDeadline, BudgetKind: "deadline"}
	if !rec.CutOff() {
		t.Fatal("fixture must be a cutoff")
	}
	if moveOffCutoff(nil, nil, rec, nil, " (1m ago)") {
		t.Fatal("a budget end moved to another agent")
	}
}
