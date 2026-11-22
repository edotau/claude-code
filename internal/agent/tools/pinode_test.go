package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func fakeMiseNode(t *testing.T, root, ver, extra string) string {
	t.Helper()
	bin := filepath.Join(root, "installs", "node", ver, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\n"+extra+"echo v"+ver+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestPickPiNode(t *testing.T) {
	ctx := context.Background()
	nvm := t.TempDir()
	old := fakeNode(t, t.TempDir(), "20.20.2")
	fakeNode(t, nvm, "22.23.2")
	nvmBin := filepath.Dir(fakeNode(t, nvm, "24.18.0"))
	fakeNode(t, nvm, "18.1.0")
	mise := t.TempDir()
	miseBin := fakeMiseNode(t, mise, "26.13.0", "")
	probes, stale := filepath.Join(t.TempDir(), "probes"), t.TempDir()
	fakeMiseNode(t, stale, "20.1.0", "echo x >> "+probes+"\n")
	if err := os.Symlink("20.1.0", filepath.Join(stale, "installs", "node", "lts")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	rows := []struct {
		name, path, nvm, mise string
		npm                   bool
		want                  string
		fail                  bool
	}{
		{name: "nvm newest", path: old, nvm: nvm, want: nvmBin},
		{name: "good PATH node", path: filepath.Join(nvmBin, "node"), want: ""},
		{name: "none new", path: old, nvm: t.TempDir(), fail: true},
		{name: "mise only", path: old, mise: mise, want: miseBin},
		{name: "nvm and mise, newest wins", path: old, nvm: nvm, mise: mise, want: miseBin},
		{name: "mise without npm skipped", path: old, mise: mise, npm: true, fail: true},
		{name: "alias and version dir", path: old, mise: stale, fail: true},
	}
	for _, r := range rows {
		got, err := pickPiNode(ctx, r.path, r.nvm, r.mise, r.npm)
		if (err != nil) != r.fail || got != r.want {
			t.Errorf("%s: got %q, %v; want %q (fail %v)", r.name, got, err, r.want, r.fail)
		}
	}
	if b, _ := os.ReadFile(probes); strings.Count(string(b), "x") != 1 {
		t.Errorf("alias and version dir probed %q, want once", b)
	}
	if err := os.WriteFile(filepath.Join(nvmBin, "npm"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := pickPiNode(ctx, filepath.Join(nvm, "versions", "node", "v22.23.2", "bin", "node"), nvm, "", true); err != nil || got != nvmBin {
		t.Errorf("PATH node without npm: got %q, %v; want the nvm node with npm %q", got, err, nvmBin)
	}
}
