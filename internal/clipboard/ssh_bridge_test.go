package clipboard

import (
	"bytes"
	"errors"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	serveOnce(t, sock, pngBytes(t))
	path, dim, err := Grab()
	if err != nil || dim != "3x2" {
		t.Fatalf("Grab = %q %q %v, want 3x2", path, dim, err)
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
	src := filepath.Join(t.TempDir(), "src.png")
	if err := os.WriteFile(src, pngBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeOsascript(t, `cp "`+src+`" "$3"; echo OK`)
	var out bytes.Buffer
	if err := servePNG(&out); err != nil {
		t.Fatal(err)
	}
	if _, err := png.DecodeConfig(&out); err != nil {
		t.Fatalf("served bytes are not a PNG: %v", err)
	}
	fakeOsascript(t, `echo NOIMAGE`)
	out.Reset()
	if err := servePNG(&out); err != nil || out.Len() != 0 {
		t.Fatalf("no image: err = %v, %d bytes; want nil, 0", err, out.Len())
	}
}

// A symlink or a plain file at the bridge path is refused before dialing, even if it leads to a live socket.
func TestReadSSHRefusesForeignPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.sock")
	serveOnce(t, real, pngBytes(t))
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.sock")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, plain} {
		if _, err := readSSH(p); !errors.Is(err, errNoBridge) {
			t.Errorf("%s: err = %v, want errNoBridge", filepath.Base(p), err)
		}
	}
}

// The plist lands under a temp HOME, launchctl (mocked) boots out then bootstraps once, and a rerun is idempotent.
func TestLaunchAgentInstall(t *testing.T) {
	home := t.TempDir()
	bindir := t.TempDir()
	log := filepath.Join(home, "launchctl.log")
	loaded := filepath.Join(home, "loaded") // present = launchd has the agent; print fails without it
	fake := "#!/bin/sh\nif [ \"$1\" = print ]; then [ -e " + loaded + " ]; exit $?; fi\necho \"$@\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(bindir, "launchctl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+":/usr/bin:/bin")
	bin := "/opt/cc/claude-code"

	var calls, reloaded []byte
	if !t.Run("install", func(t *testing.T) {
		calls = checkLaunchAgentInstall(t, home, bin, log)
	}) {
		return
	}
	if !t.Run("unloaded", func(t *testing.T) {
		reloaded = checkLaunchAgentReload(t, home, bin, log, calls)
	}) {
		return
	}
	t.Run("current", func(t *testing.T) {
		if err := os.WriteFile(loaded, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		checkLaunchAgentCurrent(t, home, bin, log, reloaded)
	})
}

func checkLaunchAgentInstall(t *testing.T, home, bin, log string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(launchPlistPath(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{bin, "<string>--serve</string>", launchSocket(home), "inetdCompatibility", launchLabel} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Dir(launchSocket(home))); err != nil {
		t.Errorf("run dir not created: %v", err)
	}
	calls, _ := os.ReadFile(log)
	if got := string(calls); got != "bootout gui/501/"+launchLabel+"\nbootstrap gui/501 "+launchPlistPath(home)+"\n" {
		t.Fatalf("launchctl calls = %q", got)
	}

	return calls
}

// Current plist, agent not loaded (booted out): rerun bootstraps it instead of reporting current.
func checkLaunchAgentReload(t *testing.T, home, bin, log string, calls []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := os.ReadFile(log)
	if want := string(calls) + "bootstrap gui/501 " + launchPlistPath(home) + "\n"; string(reloaded) != want {
		t.Fatalf("unloaded agent: launchctl calls = %q, want a bootstrap", reloaded)
	}
	return reloaded
}

func checkLaunchAgentCurrent(t *testing.T, home, bin, log string, reloaded []byte) {
	t.Helper()
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(log); !bytes.Equal(again, reloaded) {
		t.Fatalf("unchanged loaded plist reloaded: %q", again)
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("rerun output = %q", out.String())
	}
}

func TestLaunchAgentDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, "/x/claude-code", 501, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launchPlistPath(home)); err == nil {
		t.Fatal("dry run wrote the plist")
	}
}
