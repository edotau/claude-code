package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

// With --stdin-framed the prompt ends at a NUL; stdin closing after that cancels a run that is still blocked.
func TestAskStdinFramedCancelsOnEOF(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\nexec sleep 60\n")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn := os.Stdin
	os.Stdin = pr
	defer func() { os.Stdin = oldIn }()
	if _, err := pw.WriteString("do it\x00"); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		code int
		out  string
	}
	done := make(chan outcome, 1)
	go func() {
		code, out := captureAskStdout(t, "--format", "json", "--fallback", "none", "--stdin-framed")
		done <- outcome{code, out}
	}()
	time.Sleep(700 * time.Millisecond) // the run is blocked in the fake claude by now
	select {
	case o := <-done:
		t.Fatalf("run ended before stdin closed: %+v", o)
	default:
	}
	start := time.Now()
	pw.Close()
	select {
	case o := <-done:
		if o.code == 0 || !strings.Contains(o.out, `"kind":"canceled"`) {
			t.Fatalf("want a canceled failure, got code %d:\n%s", o.code, o.out)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("cancel took %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run survived stdin EOF")
	}
}
