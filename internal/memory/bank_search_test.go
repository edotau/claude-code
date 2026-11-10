package memory

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestCorpusBlocksReturnsReadErrors(t *testing.T) {
	root := isolatedConfig1(t)
	file := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, file, "## Hidden\nbody\n")
	denyBankAccess(t, file)
	hits, err := corpusBlocks(root)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("corpusBlocks = %v, %v; want file permission error", hits, err)
	}
}
