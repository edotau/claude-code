package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestShowBlockKeepsMatchBeforeUnreadableDirectory(t *testing.T) {
	isolatedConfig1(t)
	path := filepath.Join(StoreDir(), "a-match", "decisionLog.md")
	block := "## Found <!-- id:abcd1234 -->\nmatched body"
	write(t, path, block+"\n")
	unreadable := filepath.Join(StoreDir(), "z-unreadable")
	write(t, filepath.Join(unreadable, "decisionLog.md"), "## Hidden\nother body\n")
	denyBankAccess(t, unreadable)
	if _, err := os.ReadDir(unreadable); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("fixture must deny directory reads: %v", err)
	}

	gotPath, gotBlock, err := ShowBlock("abcd1234")
	if err != nil {
		t.Fatalf("later unreadable directory discarded a successful match: %v", err)
	}
	if gotPath != path {
		t.Errorf("ShowBlock path = %q, want %q", gotPath, path)
	}
	if gotBlock != block {
		t.Errorf("ShowBlock block = %q, want %q", gotBlock, block)
	}
}
