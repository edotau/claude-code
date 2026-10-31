package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// countRemoteLookups puts a git shim on PATH that logs each `remote get-url` call, and returns the count func.
func countRemoteLookups(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	shim := "#!/bin/sh\ncase \"$*\" in *\"remote get-url\"*) echo x >> '" + log + "';; esac\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "x")
	}
}

// resetRemoteCache drops the memoized remotes.
func resetRemoteCache() { remoteCache.Clear() }

// TestOriginRemoteURLIsMemoized: the second lookup for one checkout (hit or miss) forks no git; the reset hook clears it.
func TestOriginRemoteURLIsMemoized(t *testing.T) {
	tempConfig(t)
	withRemote := gitRepo(t, t.TempDir(), "has-remote")
	fixtureGit(t, withRemote, "remote", "add", "origin", "https://github.com/edotau/memo.git")
	noRemote := gitRepo(t, t.TempDir(), "no-remote")
	resetRemoteCache()
	t.Cleanup(resetRemoteCache)
	calls := countRemoteLookups(t)

	for _, c := range []struct{ dir, want string }{
		{withRemote, "https://github.com/edotau/memo.git"}, {noRemote, ""},
	} {
		before := calls()
		if got := originRemoteURL(c.dir); got != c.want {
			t.Fatalf("originRemoteURL(%s) = %q, want %q", c.dir, got, c.want)
		}
		if calls()-before != 1 {
			t.Fatalf("first lookup of %s forked git %d times, want 1", c.dir, calls()-before)
		}
		before = calls()
		if got := originRemoteURL(c.dir); got != c.want {
			t.Fatalf("cached originRemoteURL = %q, want %q", got, c.want)
		}
		if calls() != before {
			t.Fatalf("second lookup of %s forked git again", c.dir)
		}
	}

	fixtureGit(t, noRemote, "remote", "add", "origin", "https://github.com/edotau/later.git")
	if got := originRemoteURL(noRemote); got != "" {
		t.Fatalf("cache should hide the new remote, got %q", got)
	}
	resetRemoteCache()
	if got := originRemoteURL(noRemote); got != "https://github.com/edotau/later.git" {
		t.Fatalf("after reset, originRemoteURL = %q, want the new remote", got)
	}
}
