package menu

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/configure"

	"github.com/edotau/claude-code/internal/gemini"
)

func TestAgyCommandMissingVendorStartFails(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir())
	cmd := configure.AgyCommand()
	if cmd == nil {
		t.Fatal("agyCommand returned nil")
	}
	if err := cmd.Start(); err == nil {
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		t.Fatal("Start succeeded without bin/gemini")
	} else if !strings.Contains(err.Error(), filepath.Join(cfg, "bin", "gemini")) ||
		!strings.Contains(err.Error(), "not installed") {
		t.Errorf("Start error = %v, want missing bin/gemini error", err)
	}
}

// fakeAgyCommand replaces agyCommand with a shell script: setup gives it a chance to touch the marker
// (or not) before it exits, so each case drives runAgy without a real `agy` binary.
func fakeAgyCommand(t *testing.T, script string) {
	t.Helper()
	orig := configure.AgyCommand
	t.Cleanup(func() { configure.AgyCommand = orig })
	configure.AgyCommand = func() *exec.Cmd { return exec.Command("/bin/sh", "-c", script) }
}

// TestRunAgySignsInThenStopsIt: agy writes its marker after a beat and then just sits there (a stand-in
// for its own TUI staying up); runAgy must notice the marker and stop the process, not wait for its exit.
func TestRunAgySignsInThenStopsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeAgyCommand(t, "sleep 0.2; echo tok > '"+marker+"'; exec sleep 30")
	origPoll, origGrace := configure.AgySignInPoll, configure.AgyStopGrace
	t.Cleanup(func() { configure.AgySignInPoll, configure.AgyStopGrace = origPoll, origGrace })
	configure.AgySignInPoll, configure.AgyStopGrace = 50*time.Millisecond, 100*time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	done := make(chan bool, 1)
	go func() { done <- runAgy(d) }()
	select {
	case ok := <-done:
		if !ok {
			t.Errorf("runAgy = false, want true (marker appeared): %s", out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAgy did not return once the marker appeared")
	}
	if !strings.Contains(out.String(), "signed in") {
		t.Errorf("output = %q, want a signed-in line", out.String())
	}
}

// TestRunAgyReportsNotSignedInWhenItExitsOnItsOwn: agy exits without ever writing the marker.
func TestRunAgyReportsNotSignedInWhenItExitsOnItsOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeAgyCommand(t, "exit 1")
	t.Cleanup(func() { configure.AgySignInPoll = 500 * time.Millisecond })
	configure.AgySignInPoll = 20 * time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	if runAgy(d) {
		t.Errorf("runAgy = true, want false: agy exited without ever signing in (%s)", out.String())
	}
	if !strings.Contains(out.String(), "not signed in") {
		t.Errorf("output = %q, want a not-signed-in line", out.String())
	}
}

// TestRunAgyStopsOnTimeout: agy never writes the marker and never exits; runAgy must give up at
// agySignInTimeout and stop it rather than wait forever.
func TestRunAgyStopsOnTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeAgyCommand(t, "exec sleep 30")
	origPoll, origTimeout, origGrace := configure.AgySignInPoll, configure.AgySignInTimeout, configure.AgyStopGrace
	t.Cleanup(func() {
		configure.AgySignInPoll, configure.AgySignInTimeout, configure.AgyStopGrace = origPoll, origTimeout, origGrace
	})
	configure.AgySignInPoll, configure.AgySignInTimeout, configure.AgyStopGrace = 10*time.Millisecond, 60*time.Millisecond, 20*time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	done := make(chan bool, 1)
	go func() { done <- runAgy(d) }()
	select {
	case ok := <-done:
		if ok {
			t.Errorf("runAgy = true, want false (timed out): %s", out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAgy did not return at the timeout")
	}
	if !strings.Contains(out.String(), "timed out") {
		t.Errorf("output = %q, want a timed-out line", out.String())
	}
}

func TestGeminiMenuRowLabels(t *testing.T) {
	reg := menuTestEnv(t)
	m := geminiMenu(&dialog{reg: reg, out: discardWriter{}})
	var got []string
	for _, r := range m.rows {
		got = append(got, r.label)
	}
	if want := "Subscription,Login"; strings.Join(got, ",") != want {
		t.Errorf("gemini rows = %v, want %s", got, want)
	}
	if m.state.mode != modeNone {
		t.Errorf("state = %+v, want %s with nothing configured", m.state, modeNone)
	}
}
