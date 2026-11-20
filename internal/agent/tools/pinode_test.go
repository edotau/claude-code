package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func fakeNode(t *testing.T, root, ver string) string {
	t.Helper()
	bin := filepath.Join(root, "versions", "node", "v"+ver, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(bin, "node")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho v"+ver+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPickPiNode(t *testing.T) {
	nvm := t.TempDir()
	old := fakeNode(t, t.TempDir(), "20.20.2")
	fakeNode(t, nvm, "22.23.2")
	want := filepath.Dir(fakeNode(t, nvm, "24.18.0"))
	fakeNode(t, nvm, "18.1.0")
	if got, err := pickPiNode(context.Background(), old, nvm); err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	if got, err := pickPiNode(context.Background(), filepath.Join(nvm, "versions", "node", "v24.18.0", "bin", "node"), ""); err != nil || got != "" {
		t.Fatalf("new PATH node: got %q, %v", got, err)
	}
	if _, err := pickPiNode(context.Background(), old, t.TempDir()); err == nil {
		t.Fatal("want error with no new node")
	}
}
