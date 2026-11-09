package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
