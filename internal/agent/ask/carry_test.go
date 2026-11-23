package ask

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func carryAdmissionRecord(t *testing.T, flags ...string) agent.Record {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.example.com","auth":{"type":"none"}}}}}`)
	vaulttest.SetRecord(t, "codex", "profile", "dev")
	pathstest.FakeVendor(t, "codex", "exit 0")
	inner := append([]string{"claude-code", "agents", "ask", "--agent", "codex"}, flags...)
	inner = append(inner, "-")
	rec := agent.NewRecord("codex", agent.RunOptions{Carry: true, CarryArgv: inner, Prompt: "the task"}, time.Now())
	rec.Status, rec.PID, rec.Background = "running", os.Getpid(), true
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCarryRouteUsesInnerFlagsAndOverlay(t *testing.T) {
	for _, flags := range [][]string{{"--profile", "dev"}, {"--overlay", `{"provider":"databricks","profile":"dev"}`}} {
		rec := carryAdmissionRecord(t, flags...)
		got, err := carryRoute(t.Context(), rec)
		if err != nil || got.Profile != "dev" || got.Provider != "databricks" {
			t.Fatalf("%v: route = %+v, %v", flags, got, err)
		}
		if records := agent.RecentRecords(5); len(records) != 1 {
			t.Fatalf("route preview created child records: %d", len(records))
		}
	}
}

func TestCarryAdmissionBeforeChildAndTakeover(t *testing.T) {
	for _, outcome := range []serve.Outcome{serve.Admitted, serve.Direct} {
		t.Run(string(outcome), func(t *testing.T) {
			rec := carryAdmissionRecord(t, "--profile", "dev")
			oldAdmit, oldExec := admitJob, carryExec
			t.Cleanup(func() { admitJob, carryExec = oldAdmit, oldExec })
			admissions, executions := 0, 0
			admitJob = func(_ context.Context, got agent.Record, _ io.Writer) serve.Outcome {
				admissions++
				disk, err := agent.LoadRecord(rec.ID)
				if err != nil || disk.QueueLine(time.Now()) == "" || serve.PoolOf(got) != "databricks·dev" || executions != 0 {
					t.Fatalf("admission did not precede child on dev: %+v, %v, executions %d", disk, err, executions)
				}
				return outcome
			}
			carryExec = func(_ context.Context, _ []string, _ io.Reader, _ []string, _, _ io.Writer) int {
				executions++
				disk, err := agent.LoadRecord(rec.ID)
				if err != nil || disk.Progress != "" || admissions != 1 {
					t.Fatalf("child ran queued or with %d admissions: %+v, %v", admissions, disk, err)
				}
				child := agent.NewRecord("codex", agent.RunOptions{}, time.Now())
				child.Parent, child.Status, child.Cutoff = rec.ID, "stalled", agent.CutoffDeadline
				if executions == 2 {
					child.Agent, child.Status, child.Cutoff = "pi", "ok", ""
				}
				return saveChild(t, child, 0)
			}
			if code := runCarry(t.Context(), rec, io.Discard); code != 0 || admissions != 1 || executions != 2 {
				t.Fatalf("exit/admissions/children = %d/%d/%d, want 0/1/2", code, admissions, executions)
			}
		})
	}
}

func TestCarryAdmissionCancellationDoesNotStartChild(t *testing.T) {
	rec := carryAdmissionRecord(t, "--profile", "dev")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldAdmit, oldExec := admitJob, carryExec
	t.Cleanup(func() { admitJob, carryExec = oldAdmit, oldExec })
	admitJob = func(context.Context, agent.Record, io.Writer) serve.Outcome { cancel(); return serve.Direct }
	carryExec = func(context.Context, []string, io.Reader, []string, io.Writer, io.Writer) int {
		t.Fatal("cancelled admission started a child")
		return 0
	}
	if code := runCarry(ctx, rec, io.Discard); code != 1 {
		t.Fatalf("cancelled exit = %d, want 1", code)
	}
	disk, err := agent.LoadRecord(rec.ID)
	if err != nil || disk.Status == "running" || disk.Progress != "" {
		t.Fatalf("cancelled carrier left running or queued: %+v, %v", disk, err)
	}
}

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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldAdmit := admitJob
	t.Cleanup(func() { admitJob = oldAdmit })
	admitJob = func(context.Context, agent.Record, io.Writer) serve.Outcome {
		t.Fatal("foreground carry requested admission")
		return serve.Direct
	}
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
