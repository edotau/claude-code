package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestDoctorAgentVersionRows(t *testing.T) {
	hermetic(t, "")
	pathstest.FakeVendor(t, "copilot", "echo 'GitHub Copilot CLI 1.0.100.'")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "agent version copilot")
	if c == nil {
		t.Fatal("no `agent version copilot` row")
	}
	if !strings.HasPrefix(c.Detail, "warn 1.0.100 below 1.0.104") || c.Hard {
		t.Errorf("row = %+v, want detail `warn 1.0.100 below 1.0.104...` and Hard=false", *c)
	}

	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	checks, _ = DoctorChecks()
	c = findCheck(checks, "agent version copilot")
	if c == nil || c.Detail != "not installed" || c.Hard {
		t.Errorf("missing binary row = %+v, want `not installed`, non-hard", c)
	}
}

func TestToolPATHVersionProbesUnchangedBinaryOnce(t *testing.T) {
	hermetic(t, "")
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\necho x >> '" + counter + "'\necho 'fake 7.8.9'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := toolPATHVersion(bin); got != "fake 7.8.9" {
			t.Fatalf("toolPATHVersion = %q", got)
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("two lookups spawned the binary %d times, want 1", n)
	}
}
