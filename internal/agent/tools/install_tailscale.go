package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agent"
)

// tailscaleAppBin is the CLI inside the macOS app; the cask puts nothing on PATH.
var tailscaleAppBin = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// installTailscaleCask runs `brew install --cask tailscale-app`; brew itself skips an installed app.
func installTailscaleCask(ctx context.Context, w io.Writer, dryRun bool) error {
	fmt.Fprintln(w, "  exec     brew install --cask tailscale-app")
	if dryRun {
		return nil
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return errors.New("tailscale: brew not found; install Homebrew or see https://tailscale.com/download")
	}
	ctx, cancel := context.WithTimeout(ctx, installCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, brew, "install", "--cask", "tailscale-app")
	cmd.Stdout, cmd.Stderr = w, w
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent}.Environ(ctx) // brew and its cask hooks get no provider credential
	return cmd.Run()
}

// linkTailscaleCLI writes ~/.local/bin/tailscale exec'ing the app's CLI (Tailscale's documented alias, no admin).
func linkTailscaleCLI(w io.Writer, dryRun bool) error {
	local := localBinDir()
	if local == "" {
		return errors.New("tailscale: no home directory for ~/.local/bin")
	}
	dst := filepath.Join(local, "tailscale")
	if _, err := os.Stat(dst); err == nil { // Stat follows links: a dangling link is rewritten, not kept
		fmt.Fprintln(w, "  keep     "+dst)
		return nil
	}
	fmt.Fprintf(w, "  cli      %s → %s\n", dst, tailscaleAppBin)
	if dryRun {
		return nil
	}
	if _, err := os.Stat(tailscaleAppBin); err != nil {
		return fmt.Errorf("tailscale: app CLI missing after install: %w", err)
	}
	return installBinaryLocked(w, local, "tailscale", []byte("#!/bin/sh\nexec \""+tailscaleAppBin+"\" \"$@\"\n"))
}

// installTailscale is a system install, not into dir: brew cask on macOS, the official script (sudo inside) on Linux.
func installTailscale(ctx context.Context, w io.Writer, _ string, dryRun bool) error {
	switch goosOverride {
	case "linux":
		return runInstallScript(ctx, w, dryRun, tailscaleInstallScript, nil, nil)
	case "darwin":
		if err := installTailscaleCask(ctx, w, dryRun); err != nil {
			return err
		}
		return linkTailscaleCLI(w, dryRun)
	}
	return fmt.Errorf("tailscale: no installer for %s; see https://tailscale.com/download", goosOverride)
}
