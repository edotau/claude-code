package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/pathview"
)

func onWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	b, _ := os.ReadFile("/proc/version")
	return strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// Root resolves Desktop's data location without creating it; WSL always targets Windows Desktop.
func Root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return rootFor(runtime.GOOS, onWSL(), home, os.Getenv("LOCALAPPDATA"), windowsLocalAppData, pathview.Resolve)
}

func windowsLocalAppData() (string, error) {
	bin, err := exec.LookPath("cmd.exe")
	if err != nil {
		windows := os.Getenv("SystemRoot")
		if windows == "" {
			windows = os.Getenv("WINDIR")
		}
		if windows == "" {
			windows = `C:\Windows`
		}
		var ok bool
		bin, ok = pathview.Resolve(strings.TrimRight(windows, `\/`) + `\System32\cmd.exe`)
		if !ok {
			return "", fmt.Errorf("resolve Windows cmd.exe: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, bin, "/c", "echo %LOCALAPPDATA%").Output()
	if err != nil {
		return "", fmt.Errorf("resolve Windows LOCALAPPDATA: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func rootFor(goos string, wsl bool, home, local string, query func() (string, error), resolve func(string) (string, bool)) (string, error) {
	if wsl {
		win, err := query()
		if err != nil {
			return "", err
		}
		if !windowsAbsolute(win) {
			return "", errors.New("Windows LOCALAPPDATA did not resolve to an absolute Windows path")
		}
		dir, ok := resolve(win)
		if !ok || !filepath.IsAbs(dir) {
			return "", errors.New("Windows LOCALAPPDATA has no local pathview mapping")
		}
		return filepath.Join(dir, "Claude-3p"), nil
	}
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude-3p"), nil
	case "linux":
		return filepath.Join(home, ".config", "Claude-3p"), nil
	case "windows":
		if !windowsAbsolute(local) {
			return "", errors.New("LOCALAPPDATA is not an absolute Windows path")
		}
		return strings.TrimRight(local, `\/`) + `\Claude-3p`, nil
	default:
		return "", fmt.Errorf("unsupported Desktop platform %s", goos)
	}
}

// HelperSpec returns the absolute helper path, its args, and whether it is a Windows path.
func HelperSpec(profile string) (string, []string, bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, false, err
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config != "" {
		config, err = filepath.Abs(config)
		if err != nil {
			return "", nil, false, err
		}
	}
	wsl := onWSL()
	distro := os.Getenv("WSL_DISTRO_NAME")
	if wsl && distro == "" {
		distro = pathview.Default().Distro
	}
	return helperFor(runtime.GOOS, wsl, exe, distro, config, profile)
}

func helperAbsolute(p string, win bool) bool {
	if win {
		return windowsAbsolute(p)
	}
	return filepath.IsAbs(p) && !strings.ContainsAny(p, "\x00\r\n")
}

func windowsAbsolute(s string) bool {
	if strings.ContainsAny(s, "\x00\r\n") {
		return false
	}
	if strings.HasPrefix(s, `\\`) {
		parts := strings.Split(strings.TrimPrefix(s, `\\`), `\`)
		return len(parts) >= 2 && parts[0] != "" && parts[1] != ""
	}
	return len(s) >= 3 && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) &&
		s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

const wslExe = `C:\Windows\System32\wsl.exe`

func helperFor(goos string, wsl bool, exe, distro, config, profile string) (string, []string, bool, error) {
	if strings.ContainsAny(exe+distro+config+profile, "\x00\r\n") || profile == "" {
		return "", nil, false, errors.New("helper inputs are empty or contain control characters")
	}
	tail := []string{"gateway", "token", "--profile", profile}
	if wsl {
		if !strings.HasPrefix(exe, "/") || distro == "" {
			return "", nil, false, errors.New("WSL helper requires an absolute Linux executable and WSL_DISTRO_NAME")
		}
		args := []string{"--distribution", distro, "--exec", "/usr/bin/env"}
		if config != "" {
			args = append(args, "CLAUDE_CONFIG_DIR="+config)
		}
		return wslExe, append(append(args, exe), tail...), true, nil
	}
	if goos == "windows" {
		if !windowsAbsolute(exe) {
			return "", nil, false, errors.New("helper executable must be absolute")
		}
		if config != "" {
			tail = append(tail, "--config-dir", config)
		}
		return exe, tail, true, nil
	}
	if !filepath.IsAbs(exe) {
		return "", nil, false, errors.New("helper executable must be absolute")
	}
	if config != "" {
		return "/usr/bin/env", append([]string{"CLAUDE_CONFIG_DIR=" + config, exe}, tail...), false, nil
	}
	return exe, tail, false, nil
}

func targetPath(p string) (string, error) {
	if !onWSL() {
		return p, nil
	}
	win, err := pathview.ViewFrom(p, pathview.Target{Style: pathview.StyleWindows})
	if err != nil {
		return "", fmt.Errorf("render Windows target %s: %w", strconv.Quote(p), err)
	}
	return win, nil
}

// desktopRunning is a var so tests can stub the process probe.
var desktopRunning = func() bool {
	switch {
	case onWSL() || runtime.GOOS == "windows":
		out, err := exec.Command("cmd.exe", "/c", "tasklist", "/FI", "IMAGENAME eq Claude.exe", "/NH").Output()
		return err == nil && strings.Contains(strings.ToLower(string(out)), "claude.exe")
	case runtime.GOOS == "darwin":
		return exec.Command("pgrep", "-x", "Claude").Run() == nil
	}
	return false
}
