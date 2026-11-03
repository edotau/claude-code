package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	scienceAppName = "Claude Science.app"
	scienceTeamID  = "Q6L2SF6YDW"
)

// scienceRun and scienceAppRoots are seams: tests stub the macOS tools and point the app install at a temp dir.
var (
	scienceRun = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	scienceAppRoots = func() []string {
		roots := []string{"/Applications"}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			roots = append(roots, filepath.Join(home, "Applications"))
		}
		return roots
	}
)

// installScienceApp installs the signed desktop app from a DMG and links its bundled CLI into dir.
func installScienceApp(ctx context.Context, w io.Writer, dir, url string, dryRun bool) error {
	fmt.Fprintf(w, "  download %s\n", url)
	fmt.Fprintf(w, "  app      %s -> %s\n", strings.Join(scienceAppRoots(), " or "), scienceAppName)
	fmt.Fprintf(w, "  link     %s -> %s/Contents/Resources/bin/claude-science\n", filepath.Join(dir, "claude-science"), scienceAppName)
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: would download and install claude-science)")
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("install dir %q is not absolute (HOME unset?)", dir)
	}
	data, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "claude-science-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	dmg := filepath.Join(tmp, "claude-science.dmg")
	if err := os.WriteFile(dmg, data, 0o600); err != nil {
		return err
	}
	app, err := installAppFromDMG(ctx, w, dmg, tmp)
	if err != nil {
		return err
	}
	return linkScienceCLI(w, dir, app)
}

// installAppFromDMG mounts dmg read-only, verifies the app's signature, and copies it into the first writable root.
func installAppFromDMG(ctx context.Context, w io.Writer, dmg, tmp string) (string, error) {
	mnt := filepath.Join(tmp, "mnt")
	if err := os.Mkdir(mnt, 0o700); err != nil {
		return "", err
	}
	if out, err := scienceRun(ctx, "hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, dmg); err != nil {
		return "", fmt.Errorf("hdiutil attach: %w: %s", err, bytes.TrimSpace(out))
	}
	defer func() { _, _ = scienceRun(context.WithoutCancel(ctx), "hdiutil", "detach", "-force", mnt) }()
	src := filepath.Join(mnt, scienceAppName)
	if err := verifyScienceApp(ctx, src); err != nil {
		return "", err
	}
	var errs []error
	for _, root := range scienceAppRoots() {
		dest, err := replaceApp(ctx, src, root)
		if err == nil {
			fmt.Fprintf(w, "  install  %s\n", dest)
			return dest, nil
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

// verifyScienceApp refuses an app that fails codesign or is not signed by Anthropic's team.
func verifyScienceApp(ctx context.Context, app string) error {
	if out, err := scienceRun(ctx, "codesign", "--verify", "--deep", "--strict", app); err != nil {
		return fmt.Errorf("codesign verify %s: %w: %s", scienceAppName, err, bytes.TrimSpace(out))
	}
	out, err := scienceRun(ctx, "codesign", "-dv", app)
	if err != nil {
		return fmt.Errorf("codesign -dv %s: %w", scienceAppName, err)
	}
	if !bytes.Contains(out, []byte("TeamIdentifier="+scienceTeamID+"\n")) {
		return fmt.Errorf("%s is not signed by team %s; refusing to install", scienceAppName, scienceTeamID)
	}
	return nil
}

// replaceApp ditto-copies src beside root/Claude Science.app, then renames it in; the old app stays until the swap works.
func replaceApp(ctx context.Context, src, root string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(root, ".claude-science-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	staged := filepath.Join(stage, scienceAppName)
	if out, err := scienceRun(ctx, "ditto", src, staged); err != nil {
		return "", fmt.Errorf("ditto into %s: %w: %s", root, err, bytes.TrimSpace(out))
	}
	dest, old := filepath.Join(root, scienceAppName), filepath.Join(stage, "old.app")
	hadOld := os.Rename(dest, old) == nil
	if err := os.Rename(staged, dest); err != nil {
		if hadOld {
			_ = os.Rename(old, dest)
		}
		return "", err
	}
	return dest, nil
}

// linkScienceCLI points dir/claude-science at the CLI bundled in app, replacing any older file or link.
func linkScienceCLI(w io.Writer, dir, app string) error {
	target := filepath.Join(app, "Contents", "Resources", "bin", "claude-science")
	if _, err := os.Stat(target); err != nil {
		return err
	}
	link := filepath.Join(dir, "claude-science")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpLink := link + ".new"
	_ = os.Remove(tmpLink)
	if err := os.Symlink(target, tmpLink); err != nil {
		return err
	}
	if err := os.Rename(tmpLink, link); err != nil {
		return err
	}
	fmt.Fprintf(w, "  link     %s -> %s\n", link, target)
	return nil
}
