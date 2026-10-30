package clipboard

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 7, 5))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveOnce answers one connection on sock with payload, then closes (the bridge protocol).
func serveOnce(t *testing.T, sock string, payload []byte) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write(payload)
		_ = c.Close()
	}()
}

// Grab inside an SSH session reads the forwarded socket and never reaches WSLg/PowerShell.
func TestGrabSSHBridge(t *testing.T) {
	user := "cliptest" + filepath.Base(t.TempDir())
	t.Setenv("USER", user)
	t.Setenv("SSH_CONNECTION", "1.2.3.4 1 5.6.7.8 22")
	fakeWlPaste(t, `echo WSLG-TOUCHED >&2; exit 0`) // any WSLg read would be a bug
	sock := sshSocket()
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })

	if _, _, err := Grab(); !errors.Is(err, errNoBridge) {
		t.Fatalf("missing socket: err = %v, want errNoBridge", err)
	}
	_ = os.Remove(sock)
	serveOnce(t, sock, testPNG(t))
	path, dim, err := Grab()
	if err != nil || dim != "7x5" {
		t.Fatalf("Grab = %q %q %v, want 7x5", path, dim, err)
	}
	_ = os.Remove(path)
}

func TestReadSSHBadReplies(t *testing.T) {
	short := func() string { return filepath.Join(os.TempDir(), "ct"+filepath.Base(t.TempDir())+".sock") }
	empty, junk := short(), short()
	t.Cleanup(func() { _ = os.Remove(empty); _ = os.Remove(junk) })
	serveOnce(t, empty, nil)
	if _, err := readSSH(empty); !errors.Is(err, errNoSSHImage) {
		t.Fatalf("zero bytes: err = %v, want errNoSSHImage", err)
	}
	serveOnce(t, junk, []byte("not a png"))
	if _, err := readSSH(junk); err == nil {
		t.Fatal("non-PNG reply accepted")
	}
}

func TestServePNG(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.png")
	if err := os.WriteFile(src, testPNG(t), 0o644); err != nil {
		t.Fatal(err)
	}
	osa := func(body string) {
		script := "#!/bin/sh\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, "osascript"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+":/usr/bin:/bin")
	}
	osa(`cp "` + src + `" "$3"; echo OK`)
	var out bytes.Buffer
	if err := servePNG(&out); err != nil {
		t.Fatal(err)
	}
	if _, err := png.DecodeConfig(&out); err != nil {
		t.Fatalf("served bytes are not a PNG: %v", err)
	}
	osa(`echo NOIMAGE`)
	out.Reset()
	if err := servePNG(&out); err != nil || out.Len() != 0 {
		t.Fatalf("no image: err = %v, %d bytes; want nil, 0", err, out.Len())
	}
}
