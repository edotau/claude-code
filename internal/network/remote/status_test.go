package remote

import (
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
			if got, want := Normalize(c.a), Normalize(c.b); got != want {
				t.Errorf("Normalize(%q) = %q, Normalize(%q) = %q; want equal", c.a, got, c.b, want)
			}
		})
	}
}

func TestFingerprint_StableAndSensitive(t *testing.T) {
	a := Fingerprint(Normalize("hello world"))
	b := Fingerprint(Normalize("hello world"))
	if a != b {
		t.Errorf("Fingerprint not stable across identical normalized input: %q vs %q", a, b)
	}
	c := Fingerprint(Normalize("hello WORLD"))
	if a == c {
		t.Errorf("Fingerprint did not change for different content")
	}
	if len(a) != 16 {
		t.Errorf("Fingerprint length = %d, want 16 (bounded)", len(a))
	}
}

func TestClassify_WorkingIndicator(t *testing.T) {
	state, reason, _ := Classify("", paneCap("doing stuff\nesc to interrupt"))
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
		state, _, _ := Classify("", paneCap(text))
		if state != StateInputNeeded {
			t.Errorf("Classify(%q) state = %q, want %q", text, state, StateInputNeeded)
		}
	}
}

func TestClassify_ReadySettledPrompt(t *testing.T) {
	prevFP := Fingerprint(Normalize("$ "))
	state, _, _ := Classify(prevFP, paneCap("$ "))
	if state != StateReady {
		t.Errorf("state = %q, want %q", state, StateReady)
	}
}

func TestClassify_FirstObservationIsUnknownNotIdle(t *testing.T) {
	state, reason, _ := Classify("", paneCap("some static text with no prompt"))
	if state != StateUnknown {
		t.Errorf("state = %q, want %q (first observation has no baseline): %s", state, StateUnknown, reason)
	}
}

func TestClassify_StaticAmbiguousOutputIsUnknownNotIdle(t *testing.T) {
	text := "some static ambiguous text"
	prevFP := Fingerprint(Normalize(text))
	state, _, _ := Classify(prevFP, paneCap(text))
	if state != StateUnknown {
		t.Errorf("state = %q, want %q: static unchanged non-prompt text must never be reported idle", state, StateUnknown)
	}
}

func TestClassify_OutputChangedIsWorking(t *testing.T) {
	prevFP := Fingerprint(Normalize("old static text"))
	state, _, _ := Classify(prevFP, paneCap("new static text, still no prompt"))
	if state != StateWorking {
		t.Errorf("state = %q, want %q (changed output => working)", state, StateWorking)
	}
}

func TestClassify_ShellAfterAgentExit(t *testing.T) {
	c := PaneCapture{Host: "h", Session: "cc-claude-x", PaneID: "%1", Agent: "claude", CurrentCmd: "-bash", Text: "$ "}
	state, reason, _ := Classify("", c)
	if state != StateReady {
		t.Errorf("state = %q, want %q (shell after agent exit): %s", state, StateReady, reason)
	}
}

func TestClassify_InjectionShapedOutputNeverPanics(t *testing.T) {
	// Terminal text an agent might legitimately print (e.g. echoing a shell command or JSON blob)
	// shaped like shell/HTML/ANSI injection; Classify must only classify, never execute or crash.
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
					t.Errorf("Classify panicked on %q: %v", p, r)
				}
			}()
			Classify("", paneCap(p))
		}()
	}
}

func TestObserve_TypingKeepsWorkingAndAdvancesChangedAt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c1 := paneCap("esc to interrupt: typed so f")
	out := Observe(nil, []PaneCapture{c1}, nil, t0)
	if len(out) != 1 || out[0].State != StateWorking {
		t.Fatalf("first sample = %+v, want 1 working status", out)
	}
	if !out[0].ChangedAt.Equal(t0) {
		t.Errorf("ChangedAt = %v, want %v", out[0].ChangedAt, t0)
	}

	t1 := t0.Add(2 * time.Second)
	c2 := paneCap("esc to interrupt: typed so far")
	out2 := Observe(out, []PaneCapture{c2}, nil, t1)
	if out2[0].State != StateWorking {
		t.Fatalf("second sample state = %q, want %q", out2[0].State, StateWorking)
	}
	if !out2[0].ChangedAt.Equal(t0) {
		t.Errorf("ChangedAt should stay at the first working sample while state is unchanged: got %v, want %v", out2[0].ChangedAt, t0)
	}
}

func TestObserve_StartupFirstSampleIsUnknown(t *testing.T) {
	now := time.Now()
	out := Observe(nil, []PaneCapture{paneCap("some banner with no recognizable prompt")}, nil, now)
	if len(out) != 1 || out[0].State != StateUnknown {
		t.Fatalf("startup sample = %+v, want 1 unknown status", out)
	}
	if !out[0].ObservedAt.Equal(now) || !out[0].ChangedAt.Equal(now) {
		t.Errorf("ObservedAt/ChangedAt not set to now: %+v", out[0])
	}
}

func TestObserve_PaneDisappearanceReportsGone(t *testing.T) {
	t0 := time.Now()
	prev := Observe(nil, []PaneCapture{paneCap("$ ")}, nil, t0)
	t1 := t0.Add(time.Second)
	out := Observe(prev, nil, nil, t1)
	if len(out) != 1 || out[0].State != StateGone {
		t.Fatalf("after pane vanished = %+v, want 1 gone status", out)
	}
	if !out[0].ChangedAt.Equal(t1) {
		t.Errorf("ChangedAt should advance to the sample where the pane vanished: got %v, want %v", out[0].ChangedAt, t1)
	}
}

func TestObserve_SSHFailureDegradesExistingPanesToErrorWithoutDroppingThem(t *testing.T) {
	t0 := time.Now()
	prev := Observe(nil, []PaneCapture{paneCap("$ ")}, nil, t0)
	t1 := t0.Add(time.Second)
	out := Observe(prev, nil, errFakeSSH, t1)
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
	prev := Observe(nil, []PaneCapture{running}, nil, t0)
	if prev[0].State != StateWorking {
		t.Fatalf("precondition: first sample should be working, got %q", prev[0].State)
	}
	t1 := t0.Add(5 * time.Second)
	exited := PaneCapture{Host: "h", Session: "cc-claude-x", PaneID: "%1", Agent: "claude", CurrentCmd: "bash", Text: "$ "}
	out := Observe(prev, []PaneCapture{exited}, nil, t1)
	if out[0].State != StateReady {
		t.Errorf("state after agent exit = %q, want %q", out[0].State, StateReady)
	}
}

type fakeSSHErr string

func (e fakeSSHErr) Error() string { return string(e) }

var errFakeSSH = fakeSSHErr("dispatch: capture panes on h: ssh: connection refused")
