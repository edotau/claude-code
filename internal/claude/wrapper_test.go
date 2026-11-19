package claude

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
)

func TestTakeNative(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, rest := takeNative([]string{bin, "--version"}); got != bin || !slices.Equal(rest, []string{"--version"}) {
		t.Errorf("bundled path: got %q %v", got, rest)
	}
	if got, rest := takeNative([]string{"--version"}); got != "" || len(rest) != 1 {
		t.Errorf("no path: got %q %v", got, rest)
	}
	if got, _ := takeNative([]string{filepath.Join(dir, "missing", "claude")}); got != "" {
		t.Errorf("missing file taken: %q", got)
	}
}

func TestRefuseFallback(t *testing.T) {
	hermetic(t, gwProviders)
	cause := errors.New("plan broke")
	if err := refuseFallback(agent.LaunchOptions{}, cause); err != nil {
		t.Errorf("passthrough must fall back to native: %v", err)
	}
	for _, name := range []string{"gw", "nonexistent"} {
		if err := refuseFallback(agent.LaunchOptions{Provider: name}, cause); err == nil {
			t.Errorf("profile %s must be refused", name)
		}
	}
}

func TestDedupInputs(t *testing.T) {
	bin := paths.BinDir()
	t.Setenv("PATH", "/usr/bin"+string(os.PathListSeparator)+bin+string(os.PathListSeparator)+"/usr/bin")
	got, changed := dedupInputs([]string{"--settings", "a.json", "-p", "x", "--settings", "a.json", "--settings", "b.json"})
	if want := []string{"--settings", "a.json", "-p", "x", "--settings", "b.json"}; !slices.Equal(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if !slices.Equal(changed, []string{"--settings a.json", "PATH"}) || os.Getenv("PATH") != "/usr/bin" {
		t.Errorf("changed = %v, PATH = %q", changed, os.Getenv("PATH"))
	}
	if _, none := dedupInputs([]string{"-p", "x"}); len(none) != 0 {
		t.Errorf("clean input changed %v", none)
	}
}
