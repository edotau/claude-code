package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoKeyFromRemote pins the basename extraction rule (spec: store re-layout §1) across URL shapes.
func TestRepoKeyFromRemote(t *testing.T) {
	t.Parallel()
	cases := []struct{ url, want string }{
		{"ssh://git@ssh.github.com:443/edotau/claude-code.git", "claude-code"},
		{"git@github.com:edotau/claude-dbx.git", "claude-dbx"},
		{"https://github.com/edotau/claude-orcastra.git", "claude-orcastra"},
		{"https://github.com/edotau/edotau.github.io.git", "edotau.github.io"},
		{"https://github.com/edotau/claude-code", "claude-code"},      // no .git suffix
		{"https://github.com/edotau/claude-code.git/", "claude-code"}, // trailing slash
		{"", ""},
	}
	for _, c := range cases {
		if got := repoKeyFromRemote(c.url); got != c.want {
			t.Errorf("repoKeyFromRemote(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestRepoKeyUsesOriginRemote: a checkout whose origin remote basename differs from its dir name is keyed
// by the remote, not the dir — the dir name must never leak into the store key when a remote exists.
func TestRepoKeyUsesOriginRemote(t *testing.T) {
	cfg := tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "mismatched-dirname")
	fixtureGit(t, repo, "remote", "add", "origin", "https://github.com/edotau/claude-dbx.git")

	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfg, "memory", "claude-dbx")
	if dir != want {
		t.Fatalf("EnsureRepoDir(remote-keyed repo) = %s, want %s", dir, want)
	}
	o, err := ReadOrigin(dir)
	if err != nil || o.Key != "claude-dbx" {
		t.Fatalf("origin = %+v, %v", o, err)
	}
}

// TestRepoKeyFallsBackToDirBasenameWithoutRemote: no origin remote keeps the pre-spec behaviour.
func TestRepoKeyFallsBackToDirBasenameWithoutRemote(t *testing.T) {
	cfg := tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "no-remote-repo")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "memory", "no-remote-repo"); dir != want {
		t.Fatalf("EnsureRepoDir(no-remote repo) = %s, want %s", dir, want)
	}
}

// TestSameRemoteDifferentCheckoutsDoNotShareAStore: two distinct clones (different checkout paths) that
// share one origin remote URL resolve to the same slug, so the second must take a hash-suffixed key
// (existing takenKeys/slugKey collision behaviour) rather than silently writing into the first's store.
func TestSameRemoteDifferentCheckoutsDoNotShareAStore(t *testing.T) {
	cfg := tempConfig(t)
	const remote = "https://github.com/edotau/collide.git"
	a := gitRepo(t, t.TempDir(), "clone-a")
	fixtureGit(t, a, "remote", "add", "origin", remote)
	b := gitRepo(t, t.TempDir(), "clone-b")
	fixtureGit(t, b, "remote", "add", "origin", remote)

	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "memory", "collide"); da != want {
		t.Fatalf("first clone's dir = %s, want %s", da, want)
	}

	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if db == da {
		t.Fatal("second clone silently wrote into the first clone's store")
	}
	sum := sha256.Sum256([]byte(resolvePath(b)))
	wantKey := "collide--" + hex.EncodeToString(sum[:])[:8]
	if got := filepath.Base(db); got != wantKey {
		t.Fatalf("second clone's key = %q, want %q (exact key the collision logic produced)", got, wantKey)
	}
	if !strings.HasPrefix(filepath.Base(db), "collide--") {
		t.Fatalf("second clone's key %q lost the collision suffix", filepath.Base(db))
	}

	// Both stores remain independently findable, and the first's .origin was never rewritten.
	if mustRepoDir(t, a) != da || mustRepoDir(t, b) != db {
		t.Error("RepoDir must find both stores by their own checkout path")
	}
	oa, err := ReadOrigin(da)
	if err != nil || oa.Origin != resolvePath(a) {
		t.Fatalf("first store's origin was clobbered: %+v, %v", oa, err)
	}
	ob, err := ReadOrigin(db)
	if err != nil || ob.Origin != resolvePath(b) {
		t.Fatalf("second store's origin = %+v, %v, want %s", ob, err, resolvePath(b))
	}
}
