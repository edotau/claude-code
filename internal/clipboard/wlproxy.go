// Claude-aware wl-paste proxy (WSL2): the routing behind ~/.local/bin/wl-paste, run as `claude-code wl-paste`.
//
// WSLg bridges a Windows clipboard image to Wayland as image/bmp ONLY, in 32bpp BI_BITFIELDS, which Claude's
// BMP decoder rejects (anthropics/claude-code#50552). The CLI's save chain (2.1.241) is:
//
//	xclip -t image/png || wl-paste --type image/png || xclip -t image/bmp
//	  || wl-paste --type image/bmp || powershell GetImage->PNG
//
// The proxy answers BOTH wl-paste legs by transcoding the real BMP: the bmp leg gets 24bpp BI_RGB, the png
// leg gets a PNG, so paste succeeds on the native path with no PowerShell round trip. If transcoding fails
// the leg FAILS, which reaches the next leg and ultimately the PowerShell fallback. Anything else delegates.
//
// The CLI's own read is identified by its PARENT's cmdline, never by walking ancestors: ancestor-walking
// matched every descendant of a Claude session, so a user's own `wl-paste --type image/bmp` broke too. The
// CLI spawns its chain through `sh -c`, so the parent cmdline carries the whole pipeline; requiring the
// xclip leg beside the wl-paste leg keeps a user's own wl-paste untouched.
//
// Agent TUIs (gemini, opencode) spawn wl-paste directly for image/png with nothing behind it, so they
// are an ALLOWLIST; codex (native WSL detection + PowerShell fallback) and copilot (no Linux image paste)
// are deliberately absent.
//
// CLAUDE_WL_PASTE_PROXY: 0 = never interfere; reject = fail the bmp leg, leave png alone; 1 = intercept
// regardless of the parent; unset/auto = detect. Under SSH an image leg is answered from the client's
// clipboard bridge and always exits 0, since a failing leg would fall through to PowerShell.
package clipboard

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// cliChainRE matches the wl-paste bmp leg of the CLI save chain in the parent cmdline.
var cliChainRE = regexp.MustCompile(`(?s)wl-paste.*image/bmp`)

// cliLegRE is the second half of the chain match: the xclip or PowerShell leg beside the wl-paste one.
var cliLegRE = regexp.MustCompile(`(?s)xclip.*clipboard|powershell`)

// agentTUIRE matches the allowlisted agent TUIs that spawn wl-paste directly (gemini is a node script).
var agentTUIRE = regexp.MustCompile(`(?s)node.*gemini|gemini.*node|opencode`)

// proxyExec replaces the process with the real wl-paste; a var so tests never exec.
var proxyExec = proc.Exec

// proxyCall is one wl-paste invocation: the real binary, its argv and env, and the caller's streams.
type proxyCall struct {
	real           string
	args, env      []string
	stdout, stderr io.Writer
}

// RunProxy is the wl-paste proxy: it answers the agent CLIs' image legs and delegates the rest; returns the exit code.
func RunProxy(args []string, stdout, stderr io.Writer) int {
	env := proxyEnv()
	real := findRealWlPaste()
	if real == "" {
		fmt.Fprintln(stderr, "wl-paste: no real wl-paste behind the Claude clipboard proxy (is wl-clipboard installed?)")
		return 127
	}
	c := proxyCall{real: real, args: args, env: env, stdout: stdout, stderr: stderr}
	if code, ok := c.route(os.Getenv("CLAUDE_WL_PASTE_PROXY"), parentCmdline()); ok {
		return code
	}
	if err := proxyExec(real, args, env); err != nil {
		fmt.Fprintf(stderr, "wl-paste: %v\n", err)
		return 126
	}
	return 0
}

// route answers the call per mode and parent cmdline; ok is false when it should be delegated.
func (c proxyCall) route(mode, parent string) (code int, ok bool) {
	switch {
	case mode == "0":
		return 0, false
	case mode == "1" || mode == "reject":
		return c.intercept(mode == "reject")
	case cliChainRE.MatchString(parent) && cliLegRE.MatchString(parent):
		return c.intercept(false)
	case agentTUIRE.MatchString(parent):
		if code, ok := c.sshLeg(); ok {
			return code, true
		}
		if requestsType(c.args, "image/png") {
			return c.pngLeg(), true
		}
	}
	return 0, false
}

// proxyEnv reaches the WSLg socket by path when the shell lacks it; replaces an empty key, since exec takes env raw.
func proxyEnv() []string {
	env := os.Environ()
	if inSSH() || os.Getenv("WAYLAND_DISPLAY") != "" || !isSocket(wslgSocket) {
		return env
	}
	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "WAYLAND_DISPLAY=") })
	return append(env, "WAYLAND_DISPLAY="+wslgSocket)
}

// intercept routes the two image legs; ok is false when the call is neither and should be delegated.
func (c proxyCall) intercept(reject bool) (code int, ok bool) {
	if code, ok := c.sshLeg(); ok {
		return code, true
	}
	if requestsType(c.args, "image/bmp") {
		return c.bmpLeg(reject), true
	}
	if !reject && requestsType(c.args, "image/png") {
		return c.pngLeg(), true
	}
	return 0, false
}

// sshLeg answers an SSH session's image request from the client's clipboard bridge, never WSLg; always exit 0.
func (c proxyCall) sshLeg() (code int, ok bool) {
	if !inSSH() {
		return 0, false
	}
	if requestsType(c.args, "image/bmp") {
		return 0, true
	}
	if !requestsType(c.args, "image/png") {
		return 0, false
	}
	b, err := ReadSSH()
	if err == nil {
		_, err = c.stdout.Write(b)
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
	}
	return 0, true
}

// bmpLeg answers image/bmp with a decodable 24bpp BMP; reject mode fails the leg without reading.
func (c proxyCall) bmpLeg(reject bool) int {
	if reject {
		return 1
	}
	b, err := captureWlPaste(c.real, c.env, c.args)
	if err == nil {
		err = Transcode(bytes.NewReader(b), c.stdout)
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
		return 1
	}
	return 0
}

// pngLeg answers image/png: the real offer when it has one, else a PNG synthesized from the BMP offer.
func (c proxyCall) pngLeg() int {
	if b, err := captureWlPaste(c.real, c.env, c.args); err == nil && len(b) > 0 {
		if _, err := c.stdout.Write(b); err != nil {
			fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
			return 1
		}
		return 0
	}
	b, err := captureWlPaste(c.real, c.env, []string{"--type", "image/bmp"})
	if err == nil {
		err = TranscodePNG(bytes.NewReader(b), c.stdout) // decodeRaster runs before any write: no partial output
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
		return 1
	}
	return 0
}

// requestsType reports whether args ask for mime as --type=V, "--type V" or "-t V".
func requestsType(args []string, mime string) bool {
	for i, a := range args {
		switch {
		case a == "--type="+mime:
			return true
		case (a == "--type" || a == "-t") && i+1 < len(args) && args[i+1] == mime:
			return true
		}
	}
	return false
}

// parentCmdline is the spawning process's argv, space-joined; empty when /proc is unreadable.
func parentCmdline() string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", os.Getppid()))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}

// findRealWlPaste is the first executable PATH entry that is not a proxy (an empty entry means ".").
func findRealWlPaste() string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		c := filepath.Join(dir, "wl-paste")
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 && proxyState(c) == ProxyForeign {
			return c
		}
	}
	return ""
}

// isSocket reports whether path is a Unix socket.
func isSocket(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// ProxyScript is the wl-paste shim: it hands every call to `claude-code wl-paste` (RunProxy), which
// answers the agent CLIs' image/bmp and image/png legs and delegates every other call untouched.
//
//go:embed wl-paste-claude.sh
var ProxyScript []byte

// proxyMarker identifies a file as this proxy (any version), so a foreign wl-paste is never overwritten.
const proxyMarker = "Claude-aware `wl-paste` proxy for WSL2"

// procVersionPath is read to detect a WSL kernel; a var so tests can point it at a fixture.
var procVersionPath = "/proc/version"

// doctorGOOS is the OS DoctorDetail branches on; tests swap it to exercise the Linux/WSL paths on any host.
var doctorGOOS = runtime.GOOS

// OnWSLKernel reports whether /proc/version names a Microsoft (WSL) kernel.
func OnWSLKernel() bool {
	b, err := os.ReadFile(procVersionPath)
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// ProxyPath is where the proxy lives: ~/.local/bin/wl-paste, ahead of /usr/bin on a normal PATH.
func ProxyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "wl-paste")
}

// ProxyState is the proxy file's state at ProxyPath.
type ProxyState int

const (
	ProxyAbsent  ProxyState = iota // nothing at the path
	ProxyCurrent                   // byte-identical to the embedded script
	ProxyStale                     // an older proxy version
	ProxyForeign                   // some other file; never overwritten
)

// proxyState classifies the file at path.
func proxyState(path string) ProxyState {
	b, err := os.ReadFile(path)
	switch {
	case err != nil:
		return ProxyAbsent
	case bytes.Equal(b, ProxyScript):
		return ProxyCurrent
	case bytes.Contains(b, []byte(proxyMarker)):
		return ProxyStale
	default:
		return ProxyForeign
	}
}

// InstallProxy writes the wl-paste proxy to ProxyPath on WSL only; off WSL it is a silent no-op, and
// a foreign file at the path is reported and left alone.
func InstallProxy(w io.Writer, dryRun bool) error {
	if !OnWSLKernel() {
		return nil
	}
	return installProxyAt(w, ProxyPath(), dryRun)
}

// installProxyAt is InstallProxy without the WSL gate, writing atomically to dest.
func installProxyAt(w io.Writer, dest string, dryRun bool) error {
	switch proxyState(dest) {
	case ProxyCurrent:
		fmt.Fprintf(w, "  %-17s %s\n", "ok", dest+" (current)")
		return nil
	case ProxyForeign:
		fmt.Fprintf(w, "  %-17s %s\n", "skip", dest+" is not the claude-code proxy; left alone")
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", "would write", dest)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".wl-paste-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(ProxyScript); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %-17s %s\n", "wrote", dest)
	return nil
}

// DoctorDetail describes the proxy for `claude-code doctor`; ok is false only on WSL when image paste
// would go through an unproxied wl-paste.
func DoctorDetail() (ok bool, detail string) {
	if doctorGOOS == "darwin" {
		return launchDoctorDetail()
	}
	if !OnWSLKernel() {
		return true, "not WSL (proxy not needed)"
	}
	dest := ProxyPath()
	var base string
	switch proxyState(dest) {
	case ProxyAbsent:
		return false, "absent: " + dest + " (run claude-code install)"
	case ProxyForeign:
		return false, dest + " is not the proxy"
	case ProxyStale:
		return false, "stale proxy at " + dest + " (run claude-code install)"
	default:
		base = "proxy at " + dest + "; if paste fails, try claude-code clip"
	}
	if got, err := exec.LookPath("wl-paste"); err == nil && got != dest {
		return false, base + "; PATH resolves wl-paste to " + got + " first"
	}
	return true, base
}
