package tools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScienceRollbackFailureStopsFallback(t *testing.T) {
	setupToolsTestEnv(t)
	stubScienceTools(t, scienceTeamID)
	base, fallback := t.TempDir(), t.TempDir()
	prior := filepath.Join(base, scienceAppName)
	writeToolsFixture(t, filepath.Join(prior, "prior"), "original app", 0o644)
	root := filepath.Join(t.TempDir(), "Applications")
	// Moving the old app invalidates this alias, so both the swap and restore fail without races.
	symlinkToolsFixture(t, prior+string(os.PathSeparator)+"..", root)
	oldRoots := scienceAppRoots
	scienceAppRoots = func() []string { return []string{root, fallback} }
	t.Cleanup(func() { scienceAppRoots = oldRoots })

	var out bytes.Buffer
	dest, err := installAppFromDMG(context.Background(), &out, "fixture.dmg", t.TempDir())
	if dest != "" || !errors.Is(err, errAppRollback) {
		t.Fatalf("install returned %q, %v; want rollback failure instead of fallback success; output: %s", dest, err, &out)
	}
	if _, statErr := os.Stat(filepath.Join(fallback, scienceAppName)); !os.IsNotExist(statErr) {
		t.Fatalf("fallback was attempted after failed rollback: %v", statErr)
	}
	assertSciencePreviousAppPreserved(t, base, err)
}

func assertSciencePreviousAppPreserved(t *testing.T, base string, installErr error) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".claude-science-") {
		t.Fatalf("expected one preserved staging directory, got %v", entries)
	}
	old := filepath.Join(entries[0].Name(), "old.app")
	got, err := os.ReadFile(filepath.Join(base, old, "prior"))
	if err != nil || string(got) != "original app" {
		t.Fatalf("displaced app = %q, %v; want original app preserved", got, err)
	}
	if !strings.Contains(installErr.Error(), old) {
		t.Fatalf("rollback error does not identify displaced app %q: %v", old, installErr)
	}
}
