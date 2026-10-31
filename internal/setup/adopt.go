// Package setup implements the "adopt" and "nuke" setup modes shared by `install --setup`.
package setup

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
)

// Clash is a git-tracked path in the source repo that already exists at the target dir.
type Clash struct {
	Path    string      // repo-relative, slash-separated
	Size    int64       // the target's current size
	Mode    os.FileMode // the target's current mode
	Differs bool        // sha256 differs from the repo's version
}

// Clashes lists every git-tracked path in repo that already exists at dir. Untracked and ignored
// paths in dir are never clashes — they are the user's own content, never the repo's business.
func Clashes(repo, dir string) ([]Clash, error) {
	tracked, err := trackedFiles(repo)
	if err != nil {
		return nil, err
	}
	var out []Clash
	for _, rel := range tracked {
		target := filepath.Join(dir, rel)
		fi, err := os.Stat(target)
		if err != nil {
			continue // not present at dir: no clash
		}
		differs, err := filesDiffer(filepath.Join(repo, rel), target)
		if err != nil {
			return nil, err
		}
		out = append(out, Clash{Path: rel, Size: fi.Size(), Mode: fi.Mode(), Differs: differs})
	}
	return out, nil
}

// trackedFiles runs `git -C repo ls-files -z` and returns repo-relative, slash-separated paths.
func trackedFiles(repo string) ([]string, error) {
	out, err := exec.Command("git", "-C", repo, "ls-files", "-z").Output() //nolint:noctx // one-shot local git call; no caller ctx
	if err != nil {
		return nil, err
	}
	var files []string
	for p := range strings.SplitSeq(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// filesDiffer sha256-compares two files.
func filesDiffer(a, b string) (bool, error) {
	ah, err := hashFile(a)
	if err != nil {
		return false, err
	}
	bh, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(ah, bh), nil
}

func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

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
	backupDir := paths.BackupDir(label, now)
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
	if out, err := exec.Command("git", "-C", opts.Into, "checkout", "--", ".").CombinedOutput(); err != nil { //nolint:noctx // one-shot local git call; no caller ctx
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
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:noctx // one-shot local git call; no caller ctx
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
