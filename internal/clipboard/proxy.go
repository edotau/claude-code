package clipboard

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ProxyScript is the wl-paste proxy: it answers the agent CLIs' image/bmp and image/png legs by
// piping WSLg's BMP through `claude-code bmp-transcode`, and delegates every other call untouched.
//
//go:embed wl-paste-claude.sh
var ProxyScript []byte

// proxyMarker identifies a file as this proxy (any version), so a foreign wl-paste is never overwritten.
const proxyMarker = "Claude-aware `wl-paste` proxy for WSL2"

// procVersionPath is read to detect a WSL kernel; a var so tests can point it at a fixture.
var procVersionPath = "/proc/version"

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
	if runtime.GOOS == "darwin" {
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
