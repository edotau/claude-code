package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestPushedClaimRejectsFailedRemoteBranchListing(t *testing.T) {
	isolatedConfig(t)
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	write(t, git, "#!/bin/sh\nprintf '  origin/feature\\n'\nexit 1\n")
	if err := os.Chmod(git, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	repo := t.TempDir()
	v := claimVerifier{branches: map[string]string{repo: "origin/main"}}
	result, final := v.pushedIn(repo, "abc1234", verifyResult{"unverifiable", "not checked"})
	if result.status != "unverifiable" || final {
		t.Fatalf("failed branch listing with partial stdout produced %+v, final=%v", result, final)
	}
	if !strings.Contains(result.evidence, "cannot list remote branches") {
		t.Fatalf("missing branch-listing error context: %+v", result)
	}
}

// Given a symbol named only in a markdown file, When bare symbols are grepped, Then it is not ok; a Go mention is.
func TestLoadSymbolsGrepsOnlyGoFiles(t *testing.T) {
	isolatedConfig(t)
	repo := gitRepo(t, t.TempDir(), "src")
	hosttest.WriteRel(t, repo, "main.go", "package main\n\nfunc InGo() {}\n")
	hosttest.WriteRel(t, repo, "README.md", "call OnlyInDocs( here and InGo( too\n")
	hosttest.In(t, repo, "add", ".")
	hosttest.In(t, repo, "commit", "-m", "fixture")
	v := claimVerifier{store: verifyStore{repo: repo}}
	v.loadSymbols([]verifyClaim{{kind: "symbol", value: "InGo"}, {kind: "symbol", value: "OnlyInDocs"}})
	if got := v.symbols["InGo"]; got.status != "ok" || !strings.Contains(got.evidence, "main.go") {
		t.Errorf("Go symbol = %+v, want ok via main.go", got)
	}
	if got := v.symbols["OnlyInDocs"]; got.status != "unverifiable" {
		t.Errorf("symbol only in README.md = %+v, want unverifiable", got)
	}
}
