package embed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/memstore"
)

func TestEmbedCorpusReadFailureReleasesClaim(t *testing.T) {
	root := leaseFixture(t)
	bankFile := filepath.Join(memstore.HomeBankDir(), "decisionLog.md")
	backup := bankFile + ".saved"
	var calls int
	c := leaseClient(&calls, func() {
		if err := os.Rename(bankFile, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(bankFile, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	n, total, err := EmbedEntries(context.Background(), root, c)
	if !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("post-embedding corpus read must fail with EISDIR: %v", err)
	}
	wantEmbedded(t, "failed corpus read", n, calls, nil, 1, 1)
	if total != 1 {
		t.Fatalf("live entries = %d, want 1", total)
	}
	if err := os.Remove(bankFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, bankFile); err != nil {
		t.Fatal(err)
	}
	n, _, err = EmbedEntries(context.Background(), root, leaseClient(&calls, nil))
	if err != nil {
		t.Fatalf("immediate retry after corpus repair must not retain the minting claim: %v", err)
	}
	wantEmbedded(t, "immediate retry", n, calls, err, 1, 2)
}
