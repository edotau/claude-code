package clipboard

import (
	"bytes"
	"errors"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/launchd"
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
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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

// ctlStub records launchctl calls; a print fails until loaded, like launchd's "Could not find service".
type ctlStub struct {
	calls  []string
	loaded bool
}

func (s *ctlStub) run(args ...string) error {
	s.calls = append(s.calls, strings.Join(args, " "))
	if args[0] == "print" && !s.loaded {
		return errors.New("exit status 113: Could not find service")
	}
	return nil
}

// stubLaunchCtl swaps launchd.Ctl for a recorder, so a test never touches the real launchd domain.
func stubLaunchCtl(t *testing.T, s *ctlStub) {
	t.Helper()
	prev := launchd.Ctl
	launchd.Ctl = s.run
	t.Cleanup(func() { launchd.Ctl = prev })
}

// requireDarwin skips off macOS, where launchd.Install is a no-op (launchd's own tests cover that logic).
func requireDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("launchd.Install installs on macOS only")
	}
}

// The plist lands under a temp HOME, launchctl (stubbed) boots out then bootstraps once, and a rerun is idempotent.
func TestLaunchAgentInstall(t *testing.T) {
	requireDarwin(t)
	home := t.TempDir()
	stub := &ctlStub{}
	stubLaunchCtl(t, stub)
	bin := "/opt/cc/claude-code"

	if !t.Run("install", func(t *testing.T) { checkLaunchAgentInstall(t, home, bin, stub) }) {
		return
	}
	if !t.Run("unloaded", func(t *testing.T) { checkLaunchAgentReload(t, home, bin, stub) }) {
		return
	}
	stub.loaded = true
	t.Run("current", func(t *testing.T) { checkLaunchAgentCurrent(t, home, bin, stub) })
}

func checkLaunchAgentInstall(t *testing.T, home, bin string, stub *ctlStub) {
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
	want := "bootout gui/501/" + launchLabel + "\nbootstrap gui/501 " + launchPlistPath(home)
	if got := strings.Join(stub.calls, "\n"); got != want {
		t.Fatalf("launchctl calls = %q", got)
	}
}

// Current plist, agent not loaded (booted out): rerun bootstraps it instead of reporting current.
func checkLaunchAgentReload(t *testing.T, home, bin string, stub *ctlStub) {
	t.Helper()
	prev := len(stub.calls)
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	want := "print gui/501/" + launchLabel + "\nbootstrap gui/501 " + launchPlistPath(home)
	if got := strings.Join(stub.calls[prev:], "\n"); got != want {
		t.Fatalf("unloaded agent: launchctl calls = %q, want a print then bootstrap", got)
	}
}

func checkLaunchAgentCurrent(t *testing.T, home, bin string, stub *ctlStub) {
	t.Helper()
	prev := len(stub.calls)
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, bin, 501, false); err != nil {
		t.Fatal(err)
	}
	if got := stub.calls[prev:]; len(got) != 1 || got[0] != "print gui/501/"+launchLabel {
		t.Fatalf("unchanged loaded plist reloaded: %q", got)
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("rerun output = %q", out.String())
	}
}

func TestLaunchAgentDryRunWritesNothing(t *testing.T) {
	stub := &ctlStub{}
	stubLaunchCtl(t, stub)
	home := t.TempDir()
	var out bytes.Buffer
	if err := installLaunchAgentAt(&out, home, "/x/claude-code", 501, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launchPlistPath(home)); err == nil {
		t.Fatal("dry run wrote the plist")
	}
	if len(stub.calls) != 0 {
		t.Fatalf("dry run called launchctl: %q", stub.calls)
	}
}

// The rendered plist keeps the bridge wiring on every OS, and a path with & or < is XML-escaped.
func TestRenderPlistBridgeWiring(t *testing.T) {
	home := t.TempDir()
	plist, err := renderPlist("/opt/cc/claude-code", home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/opt/cc/claude-code", "<string>--serve</string>", launchSocket(home), "inetdCompatibility", launchLabel} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	esc, err := renderPlist("/opt/c&c/claude<code>", home)
	if err != nil || !strings.Contains(string(esc), "/opt/c&amp;c/claude&lt;code&gt;") {
		t.Errorf("plist not XML-escaped: %q %v", esc, err)
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
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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

func writeBridgeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedServePathRejectsSymlinkedSecrets(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	home := t.TempDir()
	for _, root := range []string{".ssh", ".gnupg", "Library/Keychains", ".claude/state"} {
		t.Run(root, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "private")
			writeBridgeFixture(t, filepath.Join(target, "secret"), "private bytes")
			link := filepath.Join(home, filepath.FromSlash(root))
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{link, target} {
				if _, _, err := allowedServePath(home, filepath.Join(dir, "secret")); err == nil || !strings.Contains(err.Error(), "not shareable") {
					t.Fatalf("secret via %s: err=%v, want refusal", dir, err)
				}
			}
		})
	}
}

// TestAllowedServePathRoots verifies only allow-listed roots (home, temp) are served and escaping symlinks fail.
func TestAllowedServePathRoots(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	home := t.TempDir()
	tmpFile := filepath.Join(t.TempDir(), "shot.png")
	writeBridgeFixture(t, tmpFile, "png bytes")
	homeFile := filepath.Join(home, "Desktop", "shot.png")
	writeBridgeFixture(t, homeFile, "png bytes")
	link := filepath.Join(home, "hosts")
	if err := os.Symlink("/etc/hosts", link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{tmpFile, homeFile} {
		if _, _, err := allowedServePath(home, p); err != nil {
			t.Errorf("%s: err=%v, want served", p, err)
		}
	}
	for _, p := range []string{"/etc/hosts", link} {
		if _, _, err := allowedServePath(home, p); err == nil || !strings.Contains(err.Error(), "not shareable") {
			t.Errorf("%s: err=%v, want not shareable", p, err)
		}
	}
}

func TestAllowedServePathRejectsUnknownHome(t *testing.T) {
	key := filepath.Join(t.TempDir(), ".ssh", "id_ed25519")
	writeBridgeFixture(t, key, "private bytes")
	t.Setenv("HOME", "")
	home, err := os.UserHomeDir()
	if err == nil || home != "" {
		t.Fatalf("home lookup = %q, %v; want failure", home, err)
	}
	if _, _, err := allowedServePath(home, key); err == nil || !strings.Contains(err.Error(), "home directory unknown") {
		t.Fatalf("unknown home: err=%v, want refusal", err)
	}
}

// On a case-insensitive filesystem, a secret dir requested with different casing is still refused.
func TestAllowedServePathCaseInsensitiveSecretDir(t *testing.T) {
	for _, root := range []string{".ssh", ".gnupg", "Library/Keychains", ".claude/state"} {
		t.Run(root, func(t *testing.T) {
			home := t.TempDir()
			writeBridgeFixture(t, filepath.Join(home, filepath.FromSlash(root), "id_ed25519"), "private bytes")
			alt := filepath.Join(home, filepath.FromSlash(strings.ToUpper(root)), "id_ed25519")
			if _, err := os.Stat(alt); os.IsNotExist(err) {
				t.Skip("filesystem is case-sensitive")
			} else if err != nil {
				t.Fatal(err)
			}
			if _, _, err := allowedServePath(home, alt); err == nil || !strings.Contains(err.Error(), "not shareable") {
				t.Fatalf("case-differing secret dir %s: err=%v, want refusal", alt, err)
			}
		})
	}
}

// A denied root reached through a differently spelled path is matched by file identity on any filesystem.
func TestUnderSecretRootByIdentity(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	dir := t.TempDir()
	denied := filepath.Join(dir, "denied")
	writeBridgeFixture(t, filepath.Join(denied, "key"), "private bytes")
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(denied, alias); err != nil {
		t.Fatal(err)
	}
	if !underSecretRoot(filepath.Join(denied, "key"), []string{alias}) {
		t.Fatal("file inside denied root not matched through an alias of that root")
	}
	if underSecretRoot(filepath.Join(denied, "key"), []string{filepath.Join(dir, "absent")}) {
		t.Fatal("unrelated missing root matched")
	}
}

func TestFetchFromPreservesSameBasename(t *testing.T) {
	home, drop := t.TempDir(), filepath.Join(t.TempDir(), "drop")
	sock := bridgeServer(t, home, nil)
	var fetched []string
	for _, content := range []string{"first report", "second report"} {
		src := filepath.Join(home, content, "report.pdf")
		writeBridgeFixture(t, src, content)
		path, err := fetchFrom(sock, src, drop)
		if err != nil {
			t.Fatal(err)
		}
		fetched = append(fetched, path)
	}
	if fetched[0] == fetched[1] {
		t.Fatal("same-basename transfers reused the first file's path")
	}
	for i, want := range []string{"first report", "second report"} {
		got, err := os.ReadFile(fetched[i])
		if err != nil || string(got) != want {
			t.Fatalf("transfer %d: bytes=%q err=%v, want %q", i, got, err, want)
		}
	}
}

type shortWriteDeadlineConn struct {
	net.Conn
	deadline time.Time
}

func (c *shortWriteDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	c.deadline = deadline
	return c.Conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
}

func TestServeConnBoundsBlockedResponse(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	conn := &shortWriteDeadlineConn{Conn: server}
	go func() { _, _ = io.WriteString(client, "bad request\n") }()
	watchdog := time.AfterFunc(time.Second, func() { _ = server.Close() })
	defer watchdog.Stop()
	start := time.Now()
	err := serveConn(io.Discard, conn, t.TempDir(), func(io.Writer) error {
		t.Error("unexpected PNG fallback")
		return nil
	})
	if !os.IsTimeout(err) {
		t.Fatalf("blocked response: err=%v, want write timeout", err)
	}
	if conn.deadline.Before(start.Add(sshTimeout)) || conn.deadline.After(time.Now().Add(sshTimeout)) {
		t.Fatalf("write deadline=%v, want sshTimeout from write", conn.deadline)
	}
}

// The install hint names the exact ssh_config lines and never writes ~/.ssh/config.
func TestPrintSSHHint(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	printSSHHint(&out, home)
	for _, s := range []string{"this Mac's ~/.ssh/config, the SSH client", "RemoteForward /tmp/claude-clip-<remote-user>.sock " + launchSocket(home), "StreamLocalBindUnlink yes"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("missing %q in %q", s, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); err == nil {
		t.Error("hint wrote under ~/.ssh")
	}
}
