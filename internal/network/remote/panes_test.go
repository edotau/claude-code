package tailscale

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func paneCap(text string) PaneCapture {
	return PaneCapture{Host: "h", Session: "cc-claude-x", PaneID: "%1", Agent: "claude", CurrentCmd: "claude", Text: text}
}

func TestNormalize_StripsANSIAndSpinnerNoise(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"SGR color codes differ, text same", "\x1b[31mhello\x1b[0m", "hello"},
		{"braille spinner frame differs", "working ⠙", "working ⠠"},
		{"elapsed-seconds counter ticks", "(12s · esc to interrupt)", "(99s · esc to interrupt)"},
		{"bare carriage return", "line\r\n", "line\n"},
		{"trailing whitespace", "line   \n", "line\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, want := normalizePane(c.a), normalizePane(c.b); got != want {
				t.Errorf("normalizePane(%q) = %q, normalizePane(%q) = %q; want equal", c.a, got, c.b, want)
			}
		})
	}
}

func TestFingerprint_StableAndSensitive(t *testing.T) {
	a := fingerprintPane(normalizePane("hello world"))
	b := fingerprintPane(normalizePane("hello world"))
	if a != b {
		t.Errorf("Fingerprint not stable across identical normalized input: %q vs %q", a, b)
	}
	c := fingerprintPane(normalizePane("hello WORLD"))
	if a == c {
		t.Errorf("Fingerprint did not change for different content")
	}
	if len(a) != 16 {
		t.Errorf("Fingerprint length = %d, want 16 (bounded)", len(a))
	}
}

func TestClassify_WorkingIndicator(t *testing.T) {
	state, reason, _ := classifyPane("", paneCap("doing stuff\nesc to interrupt"))
	if state != StateWorking {
		t.Errorf("state = %q, want %q (reason=%q)", state, StateWorking, reason)
	}
}

func TestClassify_InputNeeded(t *testing.T) {
	cases := []string{
		"Do you want to proceed? (y/n)",
		"1. Yes\n2. No",
		"╭───╮\n│ >  │\n╰───╯",
	}
	for _, text := range cases {
		state, _, _ := classifyPane("", paneCap(text))
		if state != StateInputNeeded {
			t.Errorf("classifyPane(%q) state = %q, want %q", text, state, StateInputNeeded)
		}
	}
}

func TestClassify_ReadySettledPrompt(t *testing.T) {
	prevFP := fingerprintPane(normalizePane("$ "))
	state, _, _ := classifyPane(prevFP, paneCap("$ "))
	if state != StateReady {
		t.Errorf("state = %q, want %q", state, StateReady)
	}
}

func TestClassify_FirstObservationIsUnknownNotIdle(t *testing.T) {
	state, reason, _ := classifyPane("", paneCap("some static text with no prompt"))
	if state != StateUnknown {
		t.Errorf("state = %q, want %q (first observation has no baseline): %s", state, StateUnknown, reason)
	}
}

func TestClassify_StaticAmbiguousOutputIsUnknownNotIdle(t *testing.T) {
	text := "some static ambiguous text"
	prevFP := fingerprintPane(normalizePane(text))
	state, _, _ := classifyPane(prevFP, paneCap(text))
	if state != StateUnknown {
		t.Errorf("state = %q, want %q: static unchanged non-prompt text must never be reported idle", state, StateUnknown)
	}
}

func TestClassify_OutputChangedIsWorking(t *testing.T) {
	prevFP := fingerprintPane(normalizePane("old static text"))
	state, _, _ := classifyPane(prevFP, paneCap("new static text, still no prompt"))
	if state != StateWorking {
		t.Errorf("state = %q, want %q (changed output => working)", state, StateWorking)
	}
}

func TestClassify_ShellAfterAgentExit(t *testing.T) {
	c := PaneCapture{Host: "h", Session: "cc-claude-x", PaneID: "%1", Agent: "claude", CurrentCmd: "-bash", Text: "$ "}
	state, reason, _ := classifyPane("", c)
	if state != StateReady {
		t.Errorf("state = %q, want %q (shell after agent exit): %s", state, StateReady, reason)
	}
}

func TestClassify_InjectionShapedOutputNeverPanics(t *testing.T) {
	// Terminal text an agent might legitimately print (e.g. echoing a shell command or JSON blob)
	// shaped like shell/HTML/ANSI injection; classifyPane must only classify, never execute or crash.
	payloads := []string{
		"$(rm -rf ~)\nesc to interrupt",
		"<script>alert(1)</script>",
		"\x1b]0;evil\x07$ ",
		"'; DROP TABLE panes; --",
		string([]byte{0x01, 0x02, 0x1b, '[', '3', '1', 'm'}),
	}
	for _, p := range payloads {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("classifyPane panicked on %q: %v", p, r)
				}
			}()
			classifyPane("", paneCap(p))
		}()
	}
}

func TestObserve_TypingKeepsWorkingAndAdvancesChangedAt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c1 := paneCap("esc to interrupt: typed so f")
	out := ObservePanes(nil, []PaneCapture{c1}, nil, t0)
	if len(out) != 1 || out[0].State != StateWorking {
		t.Fatalf("first sample = %+v, want 1 working status", out)
	}
	if !out[0].ChangedAt.Equal(t0) {
		t.Errorf("ChangedAt = %v, want %v", out[0].ChangedAt, t0)
	}

	t1 := t0.Add(2 * time.Second)
	c2 := paneCap("esc to interrupt: typed so far")
	out2 := ObservePanes(out, []PaneCapture{c2}, nil, t1)
	if out2[0].State != StateWorking {
		t.Fatalf("second sample state = %q, want %q", out2[0].State, StateWorking)
	}
	if !out2[0].ChangedAt.Equal(t0) {
		t.Errorf("ChangedAt should stay at the first working sample while state is unchanged: got %v, want %v", out2[0].ChangedAt, t0)
	}
}

func TestObserve_StartupFirstSampleIsUnknown(t *testing.T) {
	now := time.Now()
	out := ObservePanes(nil, []PaneCapture{paneCap("some banner with no recognizable prompt")}, nil, now)
	if len(out) != 1 || out[0].State != StateUnknown {
		t.Fatalf("startup sample = %+v, want 1 unknown status", out)
	}
	if !out[0].ObservedAt.Equal(now) || !out[0].ChangedAt.Equal(now) {
		t.Errorf("ObservedAt/ChangedAt not set to now: %+v", out[0])
	}
}

func TestObserve_PaneDisappearanceReportsGone(t *testing.T) {
	t0 := time.Now()
	prev := ObservePanes(nil, []PaneCapture{paneCap("$ ")}, nil, t0)
	t1 := t0.Add(time.Second)
	out := ObservePanes(prev, nil, nil, t1)
	if len(out) != 1 || out[0].State != StateGone {
		t.Fatalf("after pane vanished = %+v, want 1 gone status", out)
	}
	if !out[0].ChangedAt.Equal(t1) {
		t.Errorf("ChangedAt should advance to the sample where the pane vanished: got %v, want %v", out[0].ChangedAt, t1)
	}
}

func TestObserve_GonePaneIsReportedOnceThenDropped(t *testing.T) {
	t0 := time.Now()
	prev := ObservePanes(nil, []PaneCapture{paneCap("$ ")}, nil, t0)
	gone := ObservePanes(prev, nil, nil, t0.Add(time.Second))
	if out := ObservePanes(gone, nil, nil, t0.Add(2*time.Second)); len(out) != 0 {
		t.Fatalf("gone pane re-emitted on the next pass: %+v", out)
	}
}

func TestObserve_SSHFailureDegradesExistingPanesToErrorWithoutDroppingThem(t *testing.T) {
	t0 := time.Now()
	prev := ObservePanes(nil, []PaneCapture{paneCap("$ ")}, nil, t0)
	t1 := t0.Add(time.Second)
	out := ObservePanes(prev, nil, errFakeSSH, t1)
	if len(out) != 1 {
		t.Fatalf("capture error must preserve previously known panes, got %+v", out)
	}
	if out[0].State != StateError {
		t.Errorf("state = %q, want %q", out[0].State, StateError)
	}
	if out[0].Reason != errFakeSSH.Error() {
		t.Errorf("reason = %q, want %q", out[0].Reason, errFakeSSH.Error())
	}
}

func TestObserve_ShellAfterAgentExitAcrossSamples(t *testing.T) {
	t0 := time.Now()
	running := paneCap("thinking...")
	prev := ObservePanes(nil, []PaneCapture{running}, nil, t0)
	if prev[0].State != StateWorking {
		t.Fatalf("precondition: first sample should be working, got %q", prev[0].State)
	}
	t1 := t0.Add(5 * time.Second)
	exited := PaneCapture{Host: "h", Session: "cc-claude-x", PaneID: "%1", Agent: "claude", CurrentCmd: "bash", Text: "$ "}
	out := ObservePanes(prev, []PaneCapture{exited}, nil, t1)
	if out[0].State != StateReady {
		t.Errorf("state after agent exit = %q, want %q", out[0].State, StateReady)
	}
}

type fakeSSHErr string

func (e fakeSSHErr) Error() string { return string(e) }

var errFakeSSH = fakeSSHErr("dispatch: capture panes on h: ssh: connection refused")

// fakeRunner returns out/err for every call, regardless of argv, and records the argv it was called with.
func fakeRunner(out []byte, err error) (RemoteRunner, *[][]string) {
	var calls [][]string
	return func(_ context.Context, argv []string) ([]byte, error) {
		calls = append(calls, argv)
		return out, err
	}, &calls
}

// frame builds one captureCommand-shaped record: session, pane id, agent, current command, then text.
func frame(sess, pid, agent, cmd, text string) string {
	return paneRecordStart + sess + fieldSep + pid + fieldSep + agent + fieldSep + cmd + "\n" + text + paneRecordEnd + "\n"
}

func TestCapturePanes_ParsesMultipleRecords(t *testing.T) {
	blob := frame("cc-claude-a", "%1", "claude", "claude", "line1\nline2") +
		frame("cc-codex-b", "%2", "codex", "codex", "working...")
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("got %d captures, want 2: %+v", len(caps), caps)
	}
	checkParsedCaptures(t, caps)
}

// checkParsedCaptures asserts the two records of TestCapturePanes_ParsesMultipleRecords and their host.
func checkParsedCaptures(t *testing.T, caps []PaneCapture) {
	t.Helper()
	if caps[0].Session != "cc-claude-a" || caps[0].PaneID != "%1" || caps[0].Agent != "claude" || caps[0].Text != "line1\nline2" {
		t.Errorf("caps[0] = %+v, unexpected", caps[0])
	}
	if caps[1].Session != "cc-codex-b" || caps[1].Text != "working..." {
		t.Errorf("caps[1] = %+v, unexpected", caps[1])
	}
	for _, c := range caps {
		if c.Host != "myhost" {
			t.Errorf("Host = %q, want %q", c.Host, "myhost")
		}
	}
}

func TestCapturePanes_SSHFailurePropagatesError(t *testing.T) {
	wantErr := errors.New("ssh: connection refused")
	run, _ := fakeRunner(nil, wantErr)
	caps, err := CapturePanes(context.Background(), "deadhost", true, run)
	if err == nil {
		t.Fatalf("CapturePanes: want error, got nil (caps=%+v)", caps)
	}
	if !strings.Contains(err.Error(), "deadhost") || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("error = %q, want it to name the host and wrap the underlying error", err.Error())
	}
	if caps != nil {
		t.Errorf("caps = %+v, want nil on error", caps)
	}
}

func TestCapturePanes_EmptyOutputIsEmptyNotError(t *testing.T) {
	run, _ := fakeRunner([]byte(""), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("caps = %+v, want empty", caps)
	}
}

func TestCapturePanes_MalformedRecordIsSkipped(t *testing.T) {
	// Missing the newline that separates the header from the text: parseCaptureBlob must skip it,
	// not panic or misattribute the following well-formed record's text to it.
	blob := paneRecordStart + "cc-claude-a\t%1\tclaude\tclaude" + // no header/body newline
		frame("cc-codex-b", "%2", "codex", "codex", "ok")
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	for _, c := range caps {
		if c.Session == "cc-claude-a" {
			t.Errorf("malformed record should have been skipped, got %+v", c)
		}
	}
}

func TestCapturePanes_TooManyPanesIsError(t *testing.T) {
	var blob strings.Builder
	for range maxPanes + 10 {
		blob.WriteString(frame("cc-claude-x", "%1", "claude", "claude", "line"))
	}
	run, _ := fakeRunner([]byte(blob.String()), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err == nil {
		t.Fatalf("want error over %d panes, got %d captures", maxPanes, len(caps))
	}
}

func TestCapturePanes_ExactlyMaxPanesIsOK(t *testing.T) {
	var blob strings.Builder
	for range maxPanes {
		blob.WriteString(frame("cc-claude-x", "%1", "claude", "claude", "line"))
	}
	run, _ := fakeRunner([]byte(blob.String()), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil || len(caps) != maxPanes {
		t.Fatalf("got %d captures, err %v; want %d, nil", len(caps), err, maxPanes)
	}
}

func TestCapturePanes_OversizeBlobIsError(t *testing.T) {
	huge := strings.Repeat("x", maxCaptureBytes*2)
	blob := frame("cc-claude-a", "%1", "claude", "claude", huge)
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err == nil {
		t.Fatalf("want error over %d bytes, got %d captures", maxCaptureBytes, len(caps))
	}
}

func TestParseCaptureBlob_RecordWithoutEndMarkerIsSkipped(t *testing.T) {
	blob := frame("cc-claude-a", "%1", "claude", "claude", "ok") +
		paneRecordStart + "cc-codex-b" + fieldSep + "%2" + fieldSep + "codex" + fieldSep + "codex\ncut mid-te"
	caps := parseCaptureBlob("myhost", blob)
	if len(caps) != 1 || caps[0].PaneID != "%1" {
		t.Fatalf("caps = %+v, want only the terminated %%1 record", caps)
	}
}

func TestCapturePanes_InjectionShapedTextIsDataNotControl(t *testing.T) {
	// A pane legitimately echoing shell/HTML-looking text must survive as plain Text, not be
	// interpreted, split further, or crash the parser.
	payload := "$(touch /tmp/pwned); <script>alert(1)</script>; '; DROP TABLE x; --"
	blob := frame("cc-claude-a", "%1", "claude", "claude", payload)
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(caps) != 1 || caps[0].Text != payload {
		t.Fatalf("injection-shaped payload was not preserved verbatim as data: %+v", caps)
	}
}

// writeStubTmuxListPanesFails puts a `tmux` stub on dir that fails `list-panes` (simulating a
// transient tmux failure, e.g. no server at all) but would succeed on anything else.
func writeStubTmuxListPanesFails(t *testing.T, dir string) {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = list-panes ]; then echo 'simulated tmux failure' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("writeStubTmuxListPanesFails: %v", err)
	}
}

// TestCapturePanes_RealScriptPropagatesListPanesFailure is the regression test for Appendix A
// codex-review finding #11 (internal/network/tailscale/status.go): it exercises the REAL captureCommand
// shell script (via ExecRunner, run locally through sh -c since host="localhost") against a stub
// tmux whose `list-panes` fails, and requires CapturePanes to surface that as an error rather than
// a successful empty capture — which previously made every tracked pane look "gone".
func TestCapturePanes_RealScriptPropagatesListPanesFailure(t *testing.T) {
	stubDir := t.TempDir()
	writeStubTmuxListPanesFails(t, stubDir)
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	caps, err := CapturePanes(context.Background(), "localhost", true, ExecRunner)
	if err == nil {
		t.Fatalf("CapturePanes: want an error when the real script's tmux list-panes fails, got caps=%+v", caps)
	}
	if caps != nil {
		t.Errorf("caps = %+v, want nil on a propagated listing failure", caps)
	}
}

// writeStubTmuxCapturePaneFails lists one cc-* pane successfully but fails capture-pane on it, and
// touches markerPath so the test can confirm capture-pane actually ran rather than list-panes itself
// having failed first (the printf format string here must treat the pane-id field as DATA, via the
// "%s" format plus a separate argument — inlining it into the format string, e.g.
// `printf 'cc-claude-a|%1|...\n'`, makes printf itself fail on the literal "%1" token before
// list-panes's exit code is even checked, which previously made this test pass for the wrong reason).
func writeStubTmuxCapturePaneFails(t *testing.T, dir, markerPath string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  list-panes) printf '%s\\n' 'cc-claude-a|%1|claude|claude' ;;\n" +
		"  capture-pane) touch '" + markerPath + "'; echo 'simulated capture failure' >&2; exit 1 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("writeStubTmuxCapturePaneFails: %v", err)
	}
}

// TestCapturePanes_RealScriptPropagatesCapturePaneFailure covers the loop-body half of the same
// finding: a capture-pane failure for one listed pane must also fail the whole script, not just
// silently omit that pane's record. The marker-file check pins that list-panes actually succeeded and
// capture-pane was reached — without it, a broken stub that fails list-panes first would make this
// test pass vacuously, for the wrong reason (see writeStubTmuxCapturePaneFails).
func TestCapturePanes_RealScriptPropagatesCapturePaneFailure(t *testing.T) {
	stubDir := t.TempDir()
	marker := stubDir + "/capture-pane-invoked"
	writeStubTmuxCapturePaneFails(t, stubDir, marker)
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	caps, err := CapturePanes(context.Background(), "localhost", true, ExecRunner)
	if err == nil {
		t.Fatalf("CapturePanes: want an error when capture-pane fails mid-loop, got caps=%+v", caps)
	}
	if caps != nil {
		t.Errorf("caps = %+v, want nil on a propagated capture failure", caps)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("capture-pane was never invoked (list-panes must have failed first): %v", statErr)
	}
}

func TestCapturePanes_PassesBatchAndHostThroughExecArgs(t *testing.T) {
	var gotArgv []string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		gotArgv = argv
		return []byte(""), nil
	}
	if _, err := CapturePanes(context.Background(), "remote-host", true, run); err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(gotArgv) == 0 {
		t.Fatal("run was never called")
	}
	joined := strings.Join(gotArgv, " ")
	if !strings.Contains(joined, "remote-host") {
		t.Errorf("argv does not reference host: %v", gotArgv)
	}
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Errorf("batch=true should select BatchMode=yes ssh args: %v", gotArgv)
	}
}

func TestObserve_SSHFailureDoesNotReviveGonePane(t *testing.T) {
	t0 := time.Now()
	prev := ObservePanes(nil, []PaneCapture{paneCap("$ ")}, nil, t0)
	gone := ObservePanes(prev, nil, nil, t0.Add(time.Second))
	out := ObservePanes(gone, nil, errFakeSSH, t0.Add(2*time.Second))
	if len(out) != 0 {
		t.Fatalf("gone pane revived as error entry: %+v", out)
	}
}
