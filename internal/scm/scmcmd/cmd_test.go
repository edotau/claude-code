package scmcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenOriginSurfacesVaultFailure(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("BITBUCKET_URL", "https://bb.example.com")
	t.Setenv("BITBUCKET_TOKEN", "")
	if err := os.MkdirAll(filepath.Join(cfg, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "state", "vault.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://bb.example.com/scm/p/r.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", a, out)
		}
	}
	t.Chdir(repo)
	_, err := openOrigin(context.Background())
	if err == nil || strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err = %v, want the vault failure", err)
	}
}
