package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteDotDotFollowsLocalSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"real/sub", "data"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "real", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "sub"), filepath.Join(root, "data", "link")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "real", "f")
	cases := map[string]Mount{
		"nas:/export/data/link/../f":     {Point: root, Kind: KindNFS, Host: "nas", RemotePath: "/export"},
		"me@devbox:/work/data/link/../f": {Point: root, Kind: KindSSHFS, Host: "devbox", RemotePath: "/work"},
	}
	for in, m := range cases {
		r := &Resolver{Mounts: []Mount{m}}
		got, ok := r.Resolve(in)
		if !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestNinePUNCClassifiedSMB(t *testing.T) {
	ms := parseMountinfo("50 22 0:50 / /mnt/srv rw - 9p \\134\\134fileserver\\134team rw,aname=drvfs\n")
	if len(ms) != 1 || ms[0].Kind != KindSMB || ms[0].Host != "fileserver" || ms[0].RemotePath != "team" {
		t.Fatalf("mounts = %+v", ms)
	}
	r := &Resolver{Mounts: ms}
	got, ok := r.Resolve(`\\fileserver\team\file`)
	if !ok || got != "/mnt/srv/file" {
		t.Errorf("Resolve = %q, %v", got, ok)
	}
}

func TestRemoteDotDotCannotLeaveMountRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"mnt/nas", "mnt/export"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"mnt/nas/file", "mnt/export/file"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &Resolver{Mounts: []Mount{{Point: filepath.Join(root, "mnt/nas"), Kind: KindNFS, Host: "nas", RemotePath: "/export"}}}
	got, ok := r.Resolve("nas:/export/../export/file")
	if want := filepath.Join(root, "mnt/nas/file"); !ok || got != want {
		t.Errorf("Resolve = %q, %v; want %q", got, ok, want)
	}
}

func TestRemoteDotDotSymlinkCannotEscapeMount(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"nas", "other/sub"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "other", "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "other", "sub"), filepath.Join(root, "nas", "link")); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Mounts: []Mount{
		{Point: filepath.Join(root, "nas"), Kind: KindNFS, Host: "nas", RemotePath: "/export"},
		{Point: filepath.Join(root, "other"), Kind: KindOpaque},
	}}
	if got, ok := r.Resolve("nas:/export/link/../secret"); ok {
		t.Errorf("Resolve escaped the mount: %q", got)
	}
}
