package ask

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/router"
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
		{cut, true, false, router.FallbackNone, true, false},
		{cut, true, true, "all", true, false},
		{cut, true, true, router.FallbackNone, false, true},
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

// A cut-off carried child is finished by Pi under the inner line's deny, given the task and partial output.
func TestRunCarryTakesOverCutOffChild(t *testing.T) {
	const outer, child, piID = "20261122T010000.000-gemini", "20261122T010000.001-gemini", "20261122T010000.002-pi"
	inner := []string{"claude-code", "agents", "ask", "--agent", "gemini", "--tools", "--deny", "Write", "-"}
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
	t.Setenv("HARNESS_STATE_DIR", t.TempDir())
	bg := agent.Record{ID: "b", Background: true}
	out, errOut, closeLog := carryLog(bg, io.Discard)
	closeLog()
	if out != errOut {
		t.Fatal("background carry: stdout and stderr are different writers")
	}
	var answer bytes.Buffer
	fg := agent.Record{ID: "20261122T030000.000-gemini", Agent: "gemini"}
	out, errOut, closeLog = carryLog(fg, &answer)
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

// A takeover brief tells Pi to read every file it needs and to batch the independent reads into one response.
func TestTakeoverBriefAsksForOneReadBatch(t *testing.T) {
	got := fmt.Sprintf(takeoverBrief, "codex", "deadline", "the task", 4, "tail")
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

// A toolless carried run gets a toolless Pi, and an --allow rule holding a comma rides the lines as one rule.
func TestCarryLinesKeepToolsTierAndCommaRules(t *testing.T) {
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--agent", "gemini", "--allow", "Bash(foo,bar)", "--deny", "Write"}); err != nil {
		t.Fatal(err)
	}
	inner := canonicalArgv(fs, f.listValues(), "task", true, nil)
	pi, err := takeoverArgv(t.Context(), inner)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range [][]string{inner, pi} {
		if i := slices.Index(line, "--allow"); i < 0 || line[i+1] != "Bash(foo,bar)" || slices.Contains(line, "bar)") {
			t.Errorf("line %q: want --allow Bash(foo,bar) as one rule", line)
		}
	}
	if slices.Contains(pi, "--tools") || slices.Contains(pi, "--read-only") {
		t.Errorf("toolless carried run: pi line %q grants tools", pi)
	}
}

// checkTakeover asserts Pi's line keeps --tools and the inner deny, and its brief has the task and partial output.
func checkTakeover(t *testing.T, argv []string, stdin io.Reader) {
	t.Helper()
	in, _ := io.ReadAll(stdin)
	if !slices.Contains(argv, "pi") || !slices.Contains(argv, "--tools") || !slices.Contains(argv, "Write") {
		t.Errorf("takeover argv %q: want --agent pi --tools --deny Write", argv)
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
