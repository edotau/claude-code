package claude

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/paths"
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
	inline := `{"env":{"k":"v"}}`
	got, changed := dedupInputs([]string{"--settings", "a.json", "-p", "x", "--settings", "a.json", "--settings", "b.json",
		"--settings", inline, "--settings", inline})
	if want := []string{"--settings", "a.json", "-p", "x", "--settings", "b.json", "--settings", inline}; !slices.Equal(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	wantChanged := []string{"--settings a.json", "--settings <inline JSON, 17 bytes>", "PATH"}
	if !slices.Equal(changed, wantChanged) || os.Getenv("PATH") != "/usr/bin"+string(os.PathListSeparator)+bin {
		t.Errorf("changed = %v, PATH = %q", changed, os.Getenv("PATH"))
	}
	if _, none := dedupInputs([]string{"-p", "x"}); len(none) != 0 {
		t.Errorf("clean input changed %v", none)
	}
}

func TestUltrareviewArgv(t *testing.T) {
	tests := []struct {
		name    string
		u       Ultrareview
		want    []string
		wantErr string
	}{
		{"current branch", Ultrareview{}, []string{"ultrareview"}, ""},
		{"base", Ultrareview{Base: "main"}, []string{"ultrareview", "main"}, ""},
		{"pr", Ultrareview{PR: true, Focus: "12"}, []string{"ultrareview", "12"}, ""},
		{"json", Ultrareview{Base: "main", JSON: true}, []string{"ultrareview", "main", "--json"}, ""},
		{"uncommitted", Ultrareview{Uncommitted: true}, nil, "not supported"},
		{"commit", Ultrareview{Commit: "HEAD"}, nil, "not supported"},
		{"pr without number", Ultrareview{PR: true}, nil, "PR number"},
		{"base and pr", Ultrareview{Base: "main", PR: true, Focus: "1"}, nil, "at most one"},
		{"dash ref", Ultrareview{Base: "--post"}, nil, "must not start"},
		{"focus words", Ultrareview{Focus: "auth"}, nil, "no focus"},
	}
	for _, tc := range tests {
		got, err := tc.u.Argv()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err=%v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) || slices.Contains(got, "--post") {
			t.Errorf("%s: argv=%v err=%v, want %v", tc.name, got, err, tc.want)
		}
	}
}
