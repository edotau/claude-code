package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

const (
	quitWait     = 15 * time.Second
	startGrace   = 60 * time.Second
	pollInterval = 2 * time.Second
	cmdTimeout   = 30 * time.Second
	quitPoll     = 500 * time.Millisecond
)

// winLaunchScript starts the MSIX app named Claude through its AppUserModelID (unverified: no Windows host).
const winLaunchScript = `$p = Get-AppxPackage -Name Claude | Select-Object -First 1; if (-not $p) { exit 1 }; ` +
	`$a = ($p | Get-AppxPackageManifest).Package.Applications.Application | Select-Object -First 1; ` +
	`Start-Process ('shell:AppsFolder\' + $p.PackageFamilyName + '!' + $a.Id)`

// CheckRoot requires Desktop's data directory; with create, macOS makes it when the app is installed.
func CheckRoot(root string, create bool) error {
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return nil
	}
	if create && runtime.GOOS == "darwin" && !OnWSL() && macAppInstalled() {
		return os.MkdirAll(root, 0o700)
	}
	return fmt.Errorf("open Claude Desktop once first (missing directory %s)", root)
}

func macAppInstalled() bool {
	dirs := []string{"/Applications/Claude.app"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Applications", "Claude.app"))
	}
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func onWindowsHost() bool { return OnWSL() || runtime.GOOS == "windows" }

func runCmd(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}

// quitDesktop asks Desktop to close gracefully; never a force kill.
var quitDesktop = func() error {
	switch {
	case onWindowsHost():
		return runCmd("taskkill.exe", "/IM", "Claude.exe")
	case runtime.GOOS == "darwin":
		return runCmd("osascript", "-e", `quit app "Claude"`)
	}
	return fmt.Errorf("closing Claude Desktop is unsupported on %s", runtime.GOOS)
}

var launchDesktop = func() error {
	switch {
	case onWindowsHost():
		return runCmd("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", winLaunchScript)
	case runtime.GOOS == "darwin":
		return runCmd("open", "-a", "Claude")
	}
	return fmt.Errorf("launching Claude Desktop is unsupported on %s", runtime.GOOS)
}

// quitAndWait closes a running Desktop and waits up to quitWait for it to exit.
func quitAndWait() error {
	if !desktopRunning() {
		return nil
	}
	if err := quitDesktop(); err != nil {
		return fmt.Errorf("quit Claude Desktop: %w", err)
	}
	for deadline := time.Now().Add(quitWait); time.Now().Before(deadline); time.Sleep(quitPoll) {
		if !desktopRunning() {
			return nil
		}
	}
	return fmt.Errorf("desktop is still running after %s", quitWait)
}

// waitForExit blocks until ctx ends or Desktop, once seen running, exits; it errors if it never starts.
func waitForExit(ctx context.Context) error {
	start := time.Now()
	seen, misses := false, 0
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		switch {
		case desktopRunning():
			seen, misses = true, 0
		case seen:
			// One failed probe reads as "not running"; require two in a row before reverting.
			if misses++; misses >= 2 {
				return nil
			}
		case time.Since(start) > startGrace:
			return fmt.Errorf("desktop did not start within %s", startGrace)
		}
	}
}

// Session points Desktop at the proxy on profile until ctx ends or Desktop exits, then restores it.
func Session(ctx context.Context, reg *providers.Registry, profile string, out io.Writer) error {
	root, err := Root()
	if err != nil {
		return err
	}
	if err := CheckRoot(root, true); err != nil {
		return err
	}
	if desktopRunning() {
		fmt.Fprintln(out, "Claude Desktop is running; closing it.")
		if err := quitAndWait(); err != nil {
			return err
		}
	}
	opts, err := applyProfile(ctx, reg, profile, root, out)
	if err != nil {
		return err
	}
	runErr := launchDesktop()
	if runErr != nil {
		runErr = fmt.Errorf("launch Claude Desktop: %w", runErr)
	} else {
		fmt.Fprintf(out, "Desktop is on profile %s through the proxy; Ctrl+C (or quitting Desktop) puts it back.\n", opts.Profile)
		runErr = waitForExit(ctx)
	}
	return errors.Join(runErr, teardown(root, out))
}

func applyProfile(ctx context.Context, reg *providers.Registry, profile, root string, out io.Writer) (Options, error) {
	bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	opts, err := BuildOptions(bctx, reg, profile, false, Options{Root: root}, HelperSpec, proxy.Ensure)
	if err != nil {
		return opts, err
	}
	if err := Configure(opts, io.Discard); err != nil {
		return opts, err
	}
	fmt.Fprintf(out, "Applied Desktop profile %s: %s, %d models.\n", opts.Profile, opts.BaseURL, len(opts.Models))
	return opts, nil
}

// teardown quits Desktop and reverts; a quit timeout is reported but never skips the revert.
func teardown(root string, out io.Writer) error {
	restart := false
	if desktopRunning() {
		fmt.Fprintln(out, "Closing Claude Desktop.")
		if err := quitAndWait(); err != nil {
			fmt.Fprintf(out, "%v; restart Desktop after it closes to pick up the restored configuration.\n", err)
			restart = true
		}
	}
	if err := Configure(Options{Root: root, Revert: true}, io.Discard); err != nil {
		return fmt.Errorf("revert Desktop (run `claude-code configure desktop --revert`): %w", err)
	}
	if !restart {
		fmt.Fprintln(out, "Desktop is back on its previous configuration.")
	}
	return nil
}
