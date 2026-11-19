package claude

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
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
