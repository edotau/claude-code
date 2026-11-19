package ask

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
)

func TestSpecDryRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	spec := `{"agent":"claude","fallback":"none","prompt":"say ok"}`
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--spec", spec, "--dry-run"}) })
	if code != 0 || !strings.Contains(out, "agent:") || !strings.Contains(out, "argv:") || !strings.Contains(out, "say ok") {
		t.Fatalf("exit = %d, want 0 with the route block\nout=%s\nstderr=%s", code, out, stderr)
	}
	if n := len(agent.RecentRecords(2)); n != 0 {
		t.Fatalf("%d dispatch records saved, want none", n)
	}
}

func TestSpecArgsParsedNotScanned(t *testing.T) {
	_, _, err := SpecArgv(`{"prompt":"x"}}`, nil)
	if err == nil {
		t.Fatal("trailing data accepted")
	}
	if got := dropSpec([]string{"--system-prompt", "--spec", "--spec", "{}"}); len(got) != 2 || got[1] != "--spec" {
		t.Fatalf("dropSpec = %q, want the system-prompt value kept", got)
	}
	args, err := withSpec(&askFlags{spec: `{"system-prompt":"--","prompt":"--tools"}`}, nil, nil)
	if err != nil || len(args) < 4 || args[len(args)-2] != "--" || args[len(args)-1] != "--tools" {
		t.Fatalf("withSpec = %q err %v, want a terminator before the prompt", args, err)
	}
}

func TestSpecFileRefusals(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "s.json")
	if err := os.WriteFile(real, []byte(`{"prompt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l.json")
	if err := os.Symlink(real, link); err == nil {
		if _, _, err := SpecArgv(link, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink err = %v", err)
		}
	}
	big := filepath.Join(dir, "b.json")
	if err := os.WriteFile(big, []byte(`{"prompt":"`+strings.Repeat("a", 70<<10)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SpecArgv(big, nil); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("big err = %v", err)
	}
}
