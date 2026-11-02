package proxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
)

// UnitName is the systemd user unit that supervises the proxy on Linux.
const UnitName = "claude-code-proxy"

var _ = schemas.Register(schemas.Format{
	Name: "proxy-unit", Owner: "internal/proxy", Kind: schemas.Derived,
	Glob: UnitPath, Keep: "systemd user unit rendered by install; removed by nuke",
})

// systemctlTimeout bounds the short systemctl calls; stop and restart wait for the daemon's drain instead.
const systemctlTimeout = 5 * time.Second

// systemctl runs `systemctl --user args...` and returns its trimmed stdout; a var so a test can stub it.
var systemctl = func(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// UnitPath is ~/.config/systemd/user/claude-code-proxy.service (XDG_CONFIG_HOME honoured).
func UnitPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", UnitName+".service")
}

// systemdUsable reports a Linux user manager that is up (running, or degraded by some unrelated failed unit).
func systemdUsable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	out, _ := systemctl(systemctlTimeout, "is-system-running")
	return out == "running" || out == "degraded"
}

func underTest() bool {
	exe, err := os.Executable()
	return err != nil || proc.IsSelfUnderTest(exe)
}

var supervisedCache struct {
	once sync.Once
	v    bool
}

// supervised: the unit is installed and enabled, so the proxy's lifecycle goes through systemctl; cached per process.
func supervised() bool {
	supervisedCache.once.Do(func() {
		if runtime.GOOS != "linux" || underTest() {
			return
		}
		if _, err := os.Stat(UnitPath()); err != nil {
			return
		}
		_, err := systemctl(systemctlTimeout, "is-enabled", UnitName)
		supervisedCache.v = err == nil
	})
	return supervisedCache.v
}

// unitQuote makes v safe inside a double-quoted systemd Environment= assignment.
func unitQuote(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v)
}

// UnitText renders the unit for exe, mirroring the detached spawn: state dir as cwd, proxy.log, the caller's env.
func UnitText(exe string) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=claude-code model proxy\n\n[Service]\n")
	fmt.Fprintf(&b, "ExecStart=\"%s\" gateway proxy --serve\n", strings.ReplaceAll(exe, "%", "%%"))
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", strings.ReplaceAll(paths.StateDir(), "%", "%%"))
	fmt.Fprintf(&b, "StandardOutput=append:%s\nStandardError=inherit\n", strings.ReplaceAll(LogFile(), "%", "%%"))
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "PATH"} {
		if v := os.Getenv(k); v != "" {
			fmt.Fprintf(&b, "Environment=\"%s=%s\"\n", k, unitQuote(v))
		}
	}
	fmt.Fprintf(&b, "Restart=always\nRestartSec=1\nKillSignal=SIGTERM\nTimeoutStopSec=%d\n\n", int((ShutdownDrain+5*time.Second)/time.Second))
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return b.String()
}

// InstallUnit renders the unit for exe and, unless dry, writes it and enables+starts it; it returns a one-word outcome
// for the install line: "skip" (no systemd user manager), "unchanged", "installed", "updated" or "would write".
func InstallUnit(ctx context.Context, exe string, dry bool) (string, error) {
	if underTest() || !systemdUsable() {
		return "skip", nil
	}
	text, path := UnitText(exe), UnitPath()
	old, rerr := os.ReadFile(path)
	changed := rerr != nil || string(old) != text
	if dry {
		if changed {
			return "would write", nil
		}
		return "unchanged", nil
	}
	if changed {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := paths.AtomicWrite(path, []byte(text), 0o644); err != nil {
			return "", err
		}
		if _, err := systemctl(systemctlTimeout, "daemon-reload"); err != nil {
			return "", fmt.Errorf("daemon-reload: %w", err)
		}
	}
	if _, err := systemctl(systemctlTimeout, "is-enabled", UnitName); err != nil {
		// A proxy started outside the unit holds the port: drain it first, or the unit would crash-loop on bind.
		if st, ok := Running(ctx); ok {
			if err := terminate(st.PID); err == nil {
				_ = awaitPortFree(ctx, st.Port)
			}
		}
		if _, err := systemctl(ShutdownDrain+10*time.Second, "enable", "--now", UnitName); err != nil {
			return "", fmt.Errorf("enable --now: %w", err)
		}
		return "installed", nil
	}
	if changed {
		return "updated", nil
	}
	return "unchanged", nil
}

// RemoveUnit disables and deletes the unit when it belongs to config dir dir; false when there was nothing to remove.
func RemoveUnit(dir string) (bool, error) {
	if underTest() || runtime.GOOS != "linux" {
		return false, nil
	}
	b, err := os.ReadFile(UnitPath())
	if err != nil || !strings.Contains(string(b), dir) {
		return false, nil
	}
	if systemdUsable() {
		if _, err := systemctl(ShutdownDrain+10*time.Second, "disable", "--now", UnitName); err != nil {
			return false, fmt.Errorf("disable --now: %w", err)
		}
	}
	if err := os.Remove(UnitPath()); err != nil {
		return false, err
	}
	if systemdUsable() {
		_, _ = systemctl(systemctlTimeout, "daemon-reload")
	}
	return true, nil
}

// UnitStatus is the doctor detail: installed/enabled/active; ok is false when the unit is installed but not running.
func UnitStatus() (ok bool, detail string) {
	if _, err := os.Stat(UnitPath()); err != nil {
		return true, "not installed (run claude-code install)"
	}
	if !systemdUsable() {
		return true, "installed; systemd user manager unavailable"
	}
	en, _ := systemctl(systemctlTimeout, "is-enabled", UnitName)
	act, _ := systemctl(systemctlTimeout, "is-active", UnitName)
	if en == "" {
		en = "not enabled"
	}
	return act == "active" || heldNow(), "installed, " + en + ", " + act
}

func heldNow() bool {
	_, err := os.Stat(heldPath())
	return err == nil
}

// unitCtl runs start, stop or restart on the unit, waiting as long as the daemon's drain.
func unitCtl(verb string) error {
	_, err := systemctl(ShutdownDrain+10*time.Second, verb, UnitName)
	if err != nil {
		return fmt.Errorf("systemctl --user %s %s: %w", verb, UnitName, err)
	}
	return nil
}
