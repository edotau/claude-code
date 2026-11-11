package clipboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	"strings"
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

// readSSH reads one PNG from the bridge socket sock; the server writes it and closes, zero bytes meaning none.
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
	b, err := readSSH(sshSocket())
	if err != nil {
		return "", "", err
	}
	return savePNG(b)
}

// serveReqWait is how long the server waits for a request line before treating the client as an old PNG-only one.
const serveReqWait = 300 * time.Millisecond

// pngMagic opens every PNG; a fetch reply starting with it came from an old PNG-only Mac side.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// bridgeReq is the v1 request line a client sends; no line at all means "send the clipboard PNG".
type bridgeReq struct {
	V    int    `json:"v"`
	Op   string `json:"op"`
	Path string `json:"path"`
}

// ServePNG serves one bridge connection (inetd hands it over as stdin): a file request, else the clipboard PNG.
func ServePNG(w io.Writer) error {
	var in net.Conn
	if c, err := net.FileConn(os.Stdin); err == nil {
		defer func() { _ = c.Close() }()
		in = c
	}
	home, _ := os.UserHomeDir()
	return serveConn(w, in, home, func(w io.Writer) error {
		if runtime.GOOS != "darwin" {
			return errors.New("clip --serve runs on macOS only (the SSH client side)")
		}
		return servePNG(w)
	})
}

// serveConn reads one optional request line from in; a file op is answered, anything else falls back to png.
func serveConn(w io.Writer, in net.Conn, home string, png func(io.Writer) error) error {
	if in == nil {
		return png(w)
	}
	w = connWriter{in} // under launchd stdin and stdout are one socket; replies get a write deadline
	_ = in.SetReadDeadline(time.Now().Add(serveReqWait))
	line, err := bufio.NewReaderSize(in, 4096).ReadSlice('\n')
	if err != nil || len(bytes.TrimSpace(line)) == 0 {
		return png(w)
	}
	var req bridgeReq
	if json.Unmarshal(line, &req) != nil || req.Op != "file" {
		return replyErr(w, "bad request")
	}
	return serveFile(w, home, req.Path)
}

// connWriter writes to c with a fresh deadline per call, so a client that stops reading cannot block the server.
type connWriter struct{ c net.Conn }

func (cw connWriter) Write(p []byte) (int, error) {
	_ = cw.c.SetWriteDeadline(time.Now().Add(sshTimeout))
	return cw.c.Write(p)
}

// replyErr writes the one-line failure header.
func replyErr(w io.Writer, msg string) error {
	b, _ := json.Marshal(map[string]any{"ok": false, "err": msg})
	_, err := w.Write(append(b, '\n'))
	return err
}

// serveFile answers a file request: a header line, then the bytes of one allowed regular file.
func serveFile(w io.Writer, home, path string) error {
	real, size, err := allowedServePath(home, path)
	if err != nil {
		return replyErr(w, err.Error())
	}
	f, err := os.Open(real)
	if err != nil {
		return replyErr(w, "cannot open file")
	}
	defer func() { _ = f.Close() }()
	hdr, _ := json.Marshal(map[string]any{"ok": true, "size": size, "name": filepath.Base(real)})
	if _, err := w.Write(append(hdr, '\n')); err != nil {
		return err
	}
	logServed(home, real)
	_, err = io.Copy(w, io.LimitReader(f, size))
	return err
}

// allowedServePath resolves path and accepts only a regular file within the size cap outside secret directories.
func allowedServePath(home, path string) (real string, size int64, err error) {
	if !filepath.IsAbs(path) {
		return "", 0, errors.New("path must be absolute")
	}
	if home == "" {
		return "", 0, errors.New("home directory unknown: refusing file requests")
	}
	if real, err = filepath.EvalSymlinks(path); err != nil {
		return "", 0, errors.New("no such file")
	}
	fi, err := os.Stat(real)
	switch {
	case err != nil:
		return "", 0, errors.New("no such file")
	case !fi.Mode().IsRegular():
		return "", 0, errors.New("not a regular file")
	case fi.Size() > sshMaxPNG:
		return "", 0, fmt.Errorf("file over %d MiB", sshMaxPNG>>20)
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	if underSecretRoot(real, secretRoots(home)) {
		return "", 0, errors.New("path is not shareable")
	}
	return real, fi.Size(), nil
}

// underSecretRoot reports whether real is a denied root or inside one, by path string or by file identity.
func underSecretRoot(real string, roots []string) bool {
	for _, deny := range roots {
		if paths.Within(real, deny) {
			return true
		}
	}
	return ancestorIsRoot(real, roots)
}

// ancestorIsRoot reports whether an ancestor of real is os.SameFile as a denied root (case-insensitive FS safe).
func ancestorIsRoot(real string, roots []string) bool {
	var infos []os.FileInfo
	for _, r := range roots {
		if fi, err := os.Stat(r); err == nil {
			infos = append(infos, fi)
		}
	}
	for p := real; ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil {
			for _, ri := range infos {
				if os.SameFile(fi, ri) {
					return true
				}
			}
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// secretRoots lists the secret directories under home, both as named and as their symlink targets.
func secretRoots(home string) []string {
	var roots []string
	for _, d := range []string{".ssh", ".gnupg", "Library/Keychains", ".claude/state"} {
		p := filepath.Join(home, filepath.FromSlash(d))
		roots = append(roots, p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			roots = append(roots, r)
		}
	}
	return roots
}

// logServed appends the served path (never its bytes) to the clip-serve log, best effort.
func logServed(home, path string) {
	if home == "" {
		return
	}
	log := filepath.Join(home, ".claude", "state", "harness", "clip-serve.log")
	if os.MkdirAll(filepath.Dir(log), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), path)
}

// BridgeReady reports an SSH session whose forwarded clipboard socket is present and ours.
func BridgeReady() bool { return inSSH() && checkSocket(sshSocket()) == nil }

// FetchClient fetches a Mac-side file over the bridge into /tmp/claude-drop and returns the local path.
func FetchClient(path string) (local string, err error) {
	return fetchFrom(sshSocket(), path, "/tmp/claude-drop")
}

// fetchFrom is FetchClient with the socket and drop directory injected.
func fetchFrom(sock, path, dropDir string) (string, error) {
	if err := checkSocket(sock); err != nil {
		return "", err
	}
	d := net.Dialer{Timeout: sshTimeout}
	conn, err := d.DialContext(context.Background(), "unix", sock)
	if err != nil {
		return "", fmt.Errorf("%w (%s: %v)", errNoBridge, sock, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(sshTimeout))
	req, _ := json.Marshal(bridgeReq{V: 1, Op: "file", Path: path})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return "", fmt.Errorf("clipboard bridge write: %w", err)
	}
	br := bufio.NewReaderSize(conn, 4096)
	if head, _ := br.Peek(len(pngMagic)); bytes.Equal(head, pngMagic) {
		return "", errors.New("the Mac side is an old PNG-only bridge: run make install on the Mac")
	}
	line, err := br.ReadSlice('\n')
	if err != nil {
		return "", fmt.Errorf("clipboard bridge sent no file header: %w", err)
	}
	var hdr struct {
		OK   bool   `json:"ok"`
		Size int64  `json:"size"`
		Err  string `json:"err"`
	}
	if err := json.Unmarshal(line, &hdr); err != nil {
		return "", fmt.Errorf("clipboard bridge sent a bad header: %w", err)
	}
	if !hdr.OK {
		return "", fmt.Errorf("clipboard bridge refused %s: %s", path, hdr.Err)
	}
	if hdr.Size < 0 || hdr.Size > sshMaxPNG {
		return "", fmt.Errorf("clipboard bridge announced %d bytes (cap %d MiB)", hdr.Size, sshMaxPNG>>20)
	}
	data := make([]byte, hdr.Size)
	if _, err := io.ReadFull(br, data); err != nil {
		return "", fmt.Errorf("clipboard bridge file read: %w", err)
	}
	// the Mac path's basename, not the reply's name, so the remote cannot steer the write
	name := filepath.Base(strings.ReplaceAll(path, "\\", "/"))
	if name == "." || name == "/" || name == ".." {
		return "", fmt.Errorf("no file name in %q", path)
	}
	if err := os.MkdirAll(dropDir, 0o700); err != nil {
		return "", err
	}
	local, err := claimDropPath(dropDir, name)
	if err != nil {
		return "", err
	}
	if err := paths.AtomicWrite(local, data, 0o600); err != nil {
		_ = os.Remove(local)
		return "", err
	}
	return local, nil
}

// claimDropPath reserves dropDir/name, or name-N.ext when taken, so an earlier fetch is never overwritten.
func claimDropPath(dropDir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := range 1000 {
		cand := filepath.Join(dropDir, name)
		if i > 0 {
			cand = filepath.Join(dropDir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		}
		f, err := os.OpenFile(cand, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		switch {
		case err == nil:
			_ = f.Close()
			return cand, nil
		case !os.IsExist(err):
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for %q in %s", name, dropDir)
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
	if err := installLaunchAgentAt(w, home, bin, os.Getuid(), dryRun); err != nil {
		return err
	}
	if !dryRun {
		printSSHHint(w, home)
	}
	return nil
}

// printSSHHint tells the owner the remote host's ~/.ssh/config lines the bridge needs; it never writes that file.
func printSSHHint(w io.Writer, home string) {
	fmt.Fprintf(w, "  %-17s add to this Mac's ~/.ssh/config, the SSH client (replace <remote-user> with your login on the remote):\n", "clip bridge")
	fmt.Fprintf(w, "      RemoteForward /tmp/claude-clip-<remote-user>.sock %s\n      StreamLocalBindUnlink yes\n", launchSocket(home))
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
