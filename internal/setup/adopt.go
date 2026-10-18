package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
)

// Refusal errors: the cli layer maps these to exit 2 (refused), everything else to exit 1.
var (
	ErrAlreadyCheckout = errors.New("already a checkout: use git pull")
	ErrNotAClone       = errors.New("not a git checkout (missing .git)")
)

// AdoptOpts configures Adopt.
type AdoptOpts struct {
	From, Into, Label string
	DryRun            bool
	Out               io.Writer
}

// Adopt moves a staged clone's .git into Into and restores every tracked path from the index,
// after backing up whatever at Into collides with a tracked path. Into's own runtime state
// (projects/, sessions/, …) and any untracked user content are left exactly as they were.
func Adopt(opts AdoptOpts) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if _, err := os.Stat(filepath.Join(opts.Into, ".git")); err == nil {
		return ErrAlreadyCheckout
	}
	if _, err := os.Stat(filepath.Join(opts.From, ".git")); err != nil {
		return fmt.Errorf("%s: %w", opts.From, ErrNotAClone)
	}

	clashes, err := Clashes(opts.From, opts.Into)
	if err != nil {
		return err
	}
	label := opts.Label
	if label == "" {
		label = "adopt"
	}
	now := time.Now()
	backupDir := BackupPath(opts.Into, label, now)
	Report(opts.Out, clashes, backupDir)
	if opts.DryRun {
		fmt.Fprintln(opts.Out, "dry-run: no files were changed")
		return nil
	}

	if len(clashes) > 0 {
		relPaths := make([]string, len(clashes))
		for i, c := range clashes {
			relPaths[i] = c.Path
		}
		if _, err := Backup(opts.Into, relPaths, label, now); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(opts.Into, 0o755); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(opts.From, ".git"), filepath.Join(opts.Into, ".git")); err != nil {
		return err
	}
	if out, err := exec.Command("git", "-C", opts.Into, "checkout", "--", ".").CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.RemoveAll(opts.From); err != nil {
		return err
	}

	st := State{Mode: "adopt", Date: now, Commit: commitHash(opts.Into), BackupDir: backupDir, ClashCount: len(clashes)}
	if err := Record(st); err != nil {
		return err
	}
	audit.Log(audit.SetupAdopt, opts.Into, fmt.Sprintf("clashes=%d", len(clashes)))
	fmt.Fprintf(opts.Out, "\nadopted into %s\nnext:\n  make -C %s install\n  claude-code configure --check\n", opts.Into, opts.Into)
	return nil
}

func commitHash(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
