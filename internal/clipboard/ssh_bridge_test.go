package clipboard

import (
	"bytes"
	"errors"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// bridgeServer runs serveConn (home as given) on a fresh socket for each connection; png is the old-client fallback.
func bridgeServer(t *testing.T, home string, png []byte) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_ = serveConn(c, c, home, func(w io.Writer) error { _, err := w.Write(png); return err })
			}()
		}
	}()
	return sock
}

// A file dragged from the Mac is fetched through the bridge into the drop dir, byte for byte, mode 0600.
func TestFetchFromServesFile(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "My File.zip")
	want := []byte("zip bytes \x00\x01")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	drop := filepath.Join(t.TempDir(), "drop")
	got, err := fetchFrom(bridgeServer(t, home, nil), src, drop)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(got)
	fi, _ := os.Stat(got)
	if got != filepath.Join(drop, "My File.zip") || !bytes.Equal(b, want) || fi.Mode().Perm() != 0o600 {
		t.Fatalf("got %s %q %v", got, b, fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/state/harness/clip-serve.log")); err != nil {
		t.Fatalf("served path not logged: %v", err)
	}
}

// Secret directories, relative paths and symlinks into them are refused, and no file lands.
func TestFetchFromRefusals(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(home, ".ssh", "id")
	_ = os.WriteFile(key, []byte("secret"), 0o600)
	link := filepath.Join(home, "innocent")
	_ = os.Symlink(key, link)
	sock := bridgeServer(t, home, nil)
	for _, p := range []string{key, link, "rel/path", home, filepath.Join(home, "missing")} {
		drop := t.TempDir()
		if _, err := fetchFrom(sock, p, drop); err == nil {
			t.Errorf("%s: want refusal", p)
		}
		if ents, _ := os.ReadDir(drop); len(ents) != 0 {
			t.Errorf("%s: left %v in drop dir", p, ents)
		}
	}
}

// An old client sends nothing: the server still answers with the clipboard PNG after the request wait.
func TestServeConnOldClientGetsPNG(t *testing.T) {
	png := append(append([]byte{}, pngMagic...), "rest"...)
	c, err := net.Dial("unix", bridgeServer(t, t.TempDir(), png))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	got, _ := io.ReadAll(c)
	if !bytes.Equal(got, png) {
		t.Fatalf("got %q", got)
	}
}

// A new client talking to an old PNG-only server is told to run make install on the Mac.
func TestFetchFromOldServer(t *testing.T) {
	dir, _ := os.MkdirTemp("", "cb")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
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
		time.Sleep(50 * time.Millisecond) // an old server grabs the clipboard before it replies
		_, _ = c.Write(append(append([]byte{}, pngMagic...), "rest"...))
		_ = c.Close()
	}()
	_, err = fetchFrom(sock, "/x", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "make install") {
		t.Fatalf("err = %v", err)
	}
}

// No forwarded socket: errNoBridge with the RemoteForward hint.
func TestFetchFromNoBridge(t *testing.T) {
	_, err := fetchFrom(filepath.Join(t.TempDir(), "none"), "/x", t.TempDir())
	if !errors.Is(err, errNoBridge) || !strings.Contains(err.Error(), "RemoteForward") {
		t.Fatalf("err = %v", err)
	}
}

// The install hint names the exact ssh_config lines and never writes ~/.ssh/config.
func TestPrintSSHHint(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	printSSHHint(&out, home)
	for _, s := range []string{"RemoteForward /tmp/claude-clip-<remote-user>.sock " + launchSocket(home), "StreamLocalBindUnlink yes"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("missing %q in %q", s, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); err == nil {
		t.Error("hint wrote under ~/.ssh")
	}
}
