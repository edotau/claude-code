package clipboard

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"text/template"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// sshTimeout bounds one bridge round trip: the Mac side runs osascript per paste (~0.3–1s).
const sshTimeout = 10 * time.Second

// sshMaxPNG caps what the bridge may send, so a misbehaving peer cannot exhaust memory.
const sshMaxPNG = 64 << 20

// errNoBridge: inside an SSH session with no forwarded socket — never fall back to the host clipboard.
var errNoBridge = errors.New("no clipboard bridge from the SSH client")

// errNoSSHImage: the bridge answered with zero bytes, the protocol's "no image".
var errNoSSHImage = errors.New("no image in the SSH client's clipboard — copy one there first (or its bridge is down)")

// inSSH reports whether this process runs inside an SSH login, where the clipboard is the client's.
func inSSH() bool { return os.Getenv("SSH_CONNECTION") != "" }

// sshSocket is where the client's `RemoteForward /tmp/claude-clip-%r.sock` lands.
func sshSocket() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	return "/tmp/claude-clip-" + name + ".sock"
}

// ReadSSH returns the SSH client's clipboard PNG through the forwarded bridge socket.
func ReadSSH() ([]byte, error) { return readSSH(sshSocket()) }

// readSSH dials sock and reads one PNG; the server writes it and closes, zero bytes meaning none.
func readSSH(sock string) ([]byte, error) {
	if err := checkSocket(sock); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: sshTimeout}
	conn, err := d.DialContext(context.Background(), "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("%w (%s: %v)", errNoBridge, sock, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(sshTimeout))
	b, err := io.ReadAll(io.LimitReader(conn, sshMaxPNG+1))
	if err != nil {
		return nil, fmt.Errorf("clipboard bridge read: %w", err)
	}
	switch {
	case len(b) == 0:
		return nil, errNoSSHImage
	case len(b) > sshMaxPNG:
		return nil, fmt.Errorf("clipboard bridge sent over %d MiB", sshMaxPNG>>20)
	}
	if _, err := png.DecodeConfig(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("clipboard bridge sent a non-PNG: %w", err)
	}
	return b, nil
}

// checkSocket refuses a /tmp path another local user could have pre-bound: it must be our own socket.
func checkSocket(sock string) error {
	fi, err := os.Lstat(sock)
	switch {
	case err != nil:
		return fmt.Errorf("%w (%s: add RemoteForward to the client's ~/.ssh/config)", errNoBridge, sock)
	case fi.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%w (%s is not a socket)", errNoBridge, sock)
	}
	if uid, ok := paths.OwnerUID(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("%w (%s is not owned by you)", errNoBridge, sock)
	}
	return nil
}

// grabSSH saves the SSH client's clipboard image as a local PNG and returns its path plus "WxH".
func grabSSH() (path, dim string, err error) {
	b, err := ReadSSH()
	if err != nil {
		return "", "", err
	}
	return savePNG(b)
}

// ServePNG writes the macOS clipboard PNG to w for one bridge connection; no image writes nothing.
func ServePNG(w io.Writer) error {
	if runtime.GOOS != "darwin" {
		return errors.New("clip --serve runs on macOS only (the SSH client side)")
	}
	return servePNG(w)
}

// servePNG is ServePNG without the darwin gate, for tests with a faked osascript.
func servePNG(w io.Writer) error {
	path, _, err := grabMac()
	if errors.Is(err, errNoMacImage) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// launchLabel names the macOS launchd agent that serves the clipboard bridge on demand.
const launchLabel = "com.edotau.claude-code.clip"

// launchctlTimeout bounds each launchctl call so a wedged launchd cannot hang install.
const launchctlTimeout = 30 * time.Second

// plistTmpl is the socket-activated agent: launchd accepts each connection and hands it over as stdin/stdout.
var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"xml": xmlText}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{xml .Bin}}</string>
		<string>clip</string>
		<string>--serve</string>
	</array>
	<key>inetdCompatibility</key>
	<dict>
		<key>Wait</key>
		<false/>
	</dict>
	<key>Sockets</key>
	<dict>
		<key>Listeners</key>
		<dict>
			<key>SockPathName</key>
			<string>{{xml .Sock}}</string>
			<key>SockPathMode</key>
			<integer>384</integer>
		</dict>
	</dict>
</dict>
</plist>
`))

// xmlText escapes s for a plist <string> body, so a path with & or < stays well-formed.
func xmlText(s string) (string, error) {
	var b bytes.Buffer
	err := xml.EscapeText(&b, []byte(s))
	return b.String(), err
}

// launchSocket is the Unix socket launchd listens on, the target of the client's RemoteForward.
func launchSocket(home string) string { return filepath.Join(home, ".claude", "run", "clip.sock") }

// launchPlistPath is the per-user LaunchAgents file for the bridge.
func launchPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

// renderPlist returns the launchd agent definition with absolute paths.
func renderPlist(bin, home string) ([]byte, error) {
	var buf bytes.Buffer
	err := plistTmpl.Execute(&buf, struct{ Bin, Sock string }{bin, launchSocket(home)})
	return buf.Bytes(), err
}

// InstallLaunchAgent installs the on-demand clipboard bridge agent on macOS; elsewhere a silent no-op.
func InstallLaunchAgent(w io.Writer, bin string, dryRun bool) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return installLaunchAgentAt(w, home, bin, os.Getuid(), dryRun)
}

// installLaunchAgentAt is InstallLaunchAgent without the darwin gate; an unchanged plist skips the reload.
func installLaunchAgentAt(w io.Writer, home, bin string, uid int, dryRun bool) error {
	want, err := renderPlist(bin, home)
	if err != nil {
		return err
	}
	plist := launchPlistPath(home)
	domain := "gui/" + strconv.Itoa(uid)
	if have, err := os.ReadFile(plist); err == nil && bytes.Equal(have, want) {
		if launchctl("print", domain+"/"+launchLabel) == nil {
			fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "current: "+plist)
			return nil
		}
		return bootstrapAgent(w, home, domain, plist, dryRun) // plist current but not loaded
	}
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "would write "+plist+" (skipped: --dry-run)")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(launchSocket(home)), 0o755); err != nil {
		return err
	}
	if err := paths.AtomicWrite(plist, want, 0o644); err != nil {
		return err
	}
	_ = launchctl("bootout", domain+"/"+launchLabel) // not loaded yet is fine
	if err := launchctl("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "wrote "+plist)
	return nil
}

// bootstrapAgent loads an already-current plist that launchd does not have (booted out, failed bootstrap).
func bootstrapAgent(w io.Writer, home, domain, plist string, dryRun bool) error {
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "would load "+plist+" (skipped: --dry-run)")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(launchSocket(home)), 0o755); err != nil {
		return err
	}
	if err := launchctl("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	fmt.Fprintf(w, "  %-17s %s\n", "clip agent", "loaded "+plist)
	return nil
}

// launchctl runs launchctl with args, folding its output into the error.
func launchctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// launchDoctorDetail reports the macOS bridge agent: plist present and the listening socket path exists.
func launchDoctorDetail() (ok bool, detail string) {
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(launchPlistPath(home)); err != nil {
		return false, "clip bridge agent absent (run claude-code install)"
	}
	if _, err := os.Stat(launchSocket(home)); err != nil {
		return false, "clip bridge agent installed but " + launchSocket(home) + " is missing (launchctl bootstrap)"
	}
	return true, "clip bridge agent at " + launchSocket(home)
}
