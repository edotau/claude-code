package databricks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigHostOverrideOut(t *testing.T) {
	out := filepath.Join(t.TempDir(), "s.json")
	args := []string{"claude", "--host", "https://x.example.com", "--auth", "cli", "--out", out}
	if code := config(args); code != 0 {
		t.Fatalf("code %d", code)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v %v", fi, err)
	}
	if code := config(args); code != 2 {
		t.Errorf("overwrite without --force gave %d", code)
	}
	if code := config(append(args, "--force")); code != 0 {
		t.Errorf("force gave %d", code)
	}
	if code := config([]string{"claude", "--host", "https://x.example.com", "--set", "A_TOKEN=1"}); code != 2 {
		t.Errorf("credential --set gave %d", code)
	}
}

func TestScrubbed(t *testing.T) {
	got := scrubbed([]string{"HARNESS_X=1", "ANTHROPIC_BASE_URL=u", "PATH=/bin"})
	if len(got) != 1 || !strings.HasPrefix(got[0], "PATH=") {
		t.Errorf("got %v", got)
	}
}

func TestRescuePrint(t *testing.T) {
	fakeClaudeOnPath(t)
	f := &fakeChild{}
	useFake(t, f)
	code, out, _ := runCaptured(t, func() int { return rescue([]string{"--host", "https://x.example.com", "--print"}) })
	if code != 0 || len(f.calls) != 0 {
		t.Fatalf("code %d calls %v", code, f.calls)
	}
	if i := strings.Index(out, "claude --settings "); i >= 0 {
		t.Cleanup(func() { os.Remove(strings.Fields(out[i:])[2]) })
	}
}
