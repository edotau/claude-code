// Claude-aware wl-paste proxy (WSL2): the routing behind ~/.claude/bin/wl-paste, run as `claude-code clip paste`.
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
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

var cliChainRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)wl-paste.*image/bmp`) })

// cliLegRE is the second half of the chain match: the xclip or PowerShell leg beside the wl-paste one.
var cliLegRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)xclip.*clipboard|powershell`) })

// agentTUIRE matches the allowlisted agent TUIs that spawn wl-paste directly (gemini is a node script).
var agentTUIRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)node.*gemini|gemini.*node|opencode`) })

var proxyExec = proc.Exec

type proxyCall struct {
	real           string
	args, env      []string
	stdout, stderr io.Writer
}

// RunProxy is the wl-paste proxy: it answers the agent CLIs' image legs and delegates the rest; returns the exit code.
func RunProxy(args []string, stdout, stderr io.Writer) int {
	env := proxyEnv()
	c := proxyCall{real: findRealWlPaste(), args: args, env: env, stdout: stdout, stderr: stderr}
	if code, ok := c.route(os.Getenv("CLAUDE_WL_PASTE_PROXY"), parentCmdline()); ok {
		return code
	}
	if c.real == "" {
		return c.noReal()
	}
	if err := proxyExec(c.real, args, env); err != nil {
		fmt.Fprintf(stderr, "wl-paste: %v\n", err)
		return 126
	}
	return 0
}

// noReal fails a leg that needs the real wl-paste when none is installed; SSH legs never reach it.
func (c proxyCall) noReal() int {
	fmt.Fprintln(c.stderr, "wl-paste: no real wl-paste behind the Claude clipboard proxy (is wl-clipboard installed?)")
	return 127
}

func (c proxyCall) route(mode, parent string) (code int, ok bool) {
	switch {
	case mode == "0":
		return 0, false
	case mode == "1" || mode == "reject":
		return c.intercept(mode == "reject")
	case cliChainRE().MatchString(parent) && cliLegRE().MatchString(parent):
		return c.intercept(false)
	case agentTUIRE().MatchString(parent):
		if c.sshLeg() {
			return 0, true
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
	if c.sshLeg() {
		return 0, true
	}
	if requestsType(c.args, "image/bmp") {
		return c.bmpLeg(reject), true
	}
	if !reject && requestsType(c.args, "image/png") {
		return c.pngLeg(), true
	}
	return 0, false
}

// sshLeg reports whether it answered an SSH image request from the client's bridge, never WSLg; the exit is always 0.
func (c proxyCall) sshLeg() bool {
	if !inSSH() {
		return false
	}
	if requestsType(c.args, "image/bmp") {
		return true
	}
	if !requestsType(c.args, "image/png") {
		return false
	}
	b, err := readSSH(sshSocket())
	if err == nil {
		_, err = c.stdout.Write(b)
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
	}
	return true
}

// bmpLeg answers image/bmp with a decodable 24bpp BMP; reject mode fails the leg without reading.
func (c proxyCall) bmpLeg(reject bool) int {
	if c.real == "" {
		return c.noReal()
	}
	if reject {
		return 1
	}
	b, err := captureWlPaste(c.real, c.env, c.args)
	if err == nil {
		err = transcode(bytes.NewReader(b), c.stdout)
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
		return 1
	}
	return 0
}

// pngLeg answers image/png: the real offer when it has one, else a PNG synthesized from the BMP offer.
func (c proxyCall) pngLeg() int {
	if c.real == "" {
		return c.noReal()
	}
	if b, err := captureWlPaste(c.real, c.env, c.args); err == nil && len(b) > 0 {
		if _, err := c.stdout.Write(b); err != nil {
			fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
			return 1
		}
		return 0
	}
	b, err := captureWlPaste(c.real, c.env, withType(c.args, "image/bmp"))
	if err == nil {
		err = transcodePNG(bytes.NewReader(b), c.stdout) // decodeRaster runs before any write: no partial output
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "wl-paste: %v\n", err)
		return 1
	}
	return 0
}

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

// withType returns args minus any type selector, plus "--type mime"; every other selection flag (--primary) is kept.
func withType(args []string, mime string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case strings.HasPrefix(a, "--type="):
		case (a == "--type" || a == "-t") && i+1 < len(args):
			i++
		default:
			out = append(out, a)
		}
	}
	return append(out, "--type", mime)
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
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 && proxyState(c) == proxyForeign {
			return c
		}
	}
	return ""
}

// sameFile compares by inode, so a symlinked or relative PATH entry naming dest still matches.
func sameFile(a, b string) bool {
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

func isSocket(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// ProxyScript is the wl-paste shim that forwards every call to `claude-code clip paste`.
//
//go:embed wl-paste-claude.sh
var ProxyScript []byte

// proxyMarker identifies a file as this proxy (any version), so a foreign wl-paste is never overwritten.
const proxyMarker = "Claude-aware `wl-paste` proxy for WSL2"

// doctorGOOS is the OS DoctorDetail branches on; tests swap it to exercise the Linux/WSL paths on any host.
var doctorGOOS = runtime.GOOS

// proxyPath is BinDir/wl-paste: every PATH that reaches claude-code has BinDir ahead of /usr/bin, login shell or not.
func proxyPath() string { return filepath.Join(paths.BinDir(), "wl-paste") }

type proxyFileState int

const (
	proxyAbsent  proxyFileState = iota // nothing at the path
	proxyCurrent                       // byte-identical to the embedded script
	proxyStale                         // an older proxy version
	proxyForeign                       // some other file; never overwritten
)

func proxyState(path string) proxyFileState {
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return proxyAbsent
	case err != nil:
		return proxyForeign // unreadable: never overwrite what we cannot identify
	case bytes.Equal(b, ProxyScript):
		return proxyCurrent
	case bytes.Contains(b, []byte(proxyMarker)):
		return proxyStale
	default:
		return proxyForeign
	}
}

// InstallProxy writes the wl-paste proxy to proxyPath on WSL only (else a silent no-op); a foreign file is left alone.
func InstallProxy(w io.Writer, dryRun bool) error {
	if !onWSL() {
		return nil
	}
	if err := installProxyAt(w, proxyPath(), dryRun); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("wl-paste proxy path: %w", err)
	}
	return removeLegacyProxy(w, filepath.Join(home, ".local", "bin", "wl-paste"), dryRun)
}

// removeLegacyProxy deletes our proxy from its pre-BinDir home; a foreign wl-paste there is never touched.
func removeLegacyProxy(w io.Writer, legacy string, dryRun bool) error {
	if st := proxyState(legacy); st != proxyCurrent && st != proxyStale {
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", "would remove", legacy)
		return nil
	}
	if err := os.Remove(legacy); err != nil {
		return fmt.Errorf("remove old wl-paste proxy: %w", err)
	}
	fmt.Fprintf(w, "  %-17s %s\n", "removed", legacy)
	return nil
}

func installProxyAt(w io.Writer, dest string, dryRun bool) error {
	switch proxyState(dest) {
	case proxyCurrent:
		fmt.Fprintf(w, "  %-17s %s\n", "ok", dest+" (current)")
		return nil
	case proxyForeign:
		fmt.Fprintf(w, "  %-17s %s\n", "skip", dest+" is not the claude-code proxy; left alone")
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "  %-17s %s\n", "would write", dest)
		return nil
	}
	if err := paths.AtomicWrite(dest, ProxyScript, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %-17s %s\n", "wrote", dest)
	return nil
}

// DoctorDetail reports clipboard proxy health; ok is false when WSL image paste or macOS bridge is unconfigured.
func DoctorDetail() (ok bool, detail string) {
	if doctorGOOS == "darwin" {
		return launchDoctorDetail()
	}
	if !onWSL() {
		return true, "not WSL (proxy not needed)"
	}
	dest := proxyPath()
	var base string
	switch proxyState(dest) {
	case proxyAbsent:
		return false, "absent: " + dest + " (run claude-code install)"
	case proxyForeign:
		return false, dest + " is not the proxy"
	case proxyStale:
		return false, "stale proxy at " + dest + " (run claude-code install)"
	default:
		base = "proxy at " + dest + "; if paste fails, try claude-code clip"
	}
	if got, err := exec.LookPath("wl-paste"); err == nil && !sameFile(got, dest) {
		return false, base + "; PATH resolves wl-paste to " + got + " first (put " + filepath.Dir(dest) + " ahead)"
	}
	return true, base
}
