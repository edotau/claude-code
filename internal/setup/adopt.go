// Package setup implements `install` and its `--setup` modes (adopt, nuke, workspace) and the `clean` sweep.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// clash is a git-tracked path in the source repo that already exists at the target dir.
type clash struct {
	Path    string // repo-relative, slash-separated
	Differs bool   // content differs from the repo's version
}

// clashes lists every git-tracked path in repo that already exists at dir. Untracked and ignored
// paths in dir are never clashes — they are the user's own content, never the repo's business.
func clashes(repo, dir string) ([]clash, error) {
	tracked, err := trackedFiles(repo)
	if err != nil {
		return nil, err
	}
	var out []clash
	for _, rel := range tracked {
		target := filepath.Join(dir, rel)
		fi, err := os.Lstat(target)
		if errors.Is(err, fs.ErrNotExist) {
			continue // not present at dir: no clash
		}
		if err != nil {
			return nil, err
		}
		out = append(out, clash{Path: rel, Differs: clashDiffers(filepath.Join(repo, rel), target, fi)})
	}
	return out, nil
}

// clashDiffers reports whether target differs from the repo's copy at src; a directory or dangling link always does.
func clashDiffers(src, target string, fi os.FileInfo) bool {
	if fi.IsDir() {
		return true
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if _, err := os.Stat(target); err != nil {
			return true
		}
	}
	return !sameBytes(src, target) // an unreadable file counts as differing, so it is backed up
}

// trackedFiles runs `git -C repo ls-files -z` and returns repo-relative, slash-separated paths.
func trackedFiles(repo string) ([]string, error) {
	out, err := exec.Command("git", "-C", repo, "ls-files", "-z").Output() //nolint:noctx // one-shot local git call; no caller ctx
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// splitNUL splits NUL-terminated `git ls-files -z` output, dropping empty entries.
func splitNUL(out []byte) []string {
	var files []string
	for p := range strings.SplitSeq(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files
}

// Refusal errors: the cli layer maps these to exit 2 (refused), everything else to exit 1.
var (
	errAlreadyCheckout = errors.New("already a checkout: use git pull")
	errNotAClone       = errors.New("not a git checkout (missing .git)")
)

// adoptOpts configures adopt.
type adoptOpts struct {
	From, Into, Label string
	DryRun            bool
	Out               io.Writer
}

// adopt moves a staged clone's .git into Into and restores every tracked path from the index,
// after backing up whatever at Into collides with a tracked path. Into's own runtime state
// (projects/, sessions/, …) and any untracked user content are left exactly as they were.
func adopt(opts adoptOpts) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if err := adoptPreflight(opts); err != nil {
		return err
	}

	clashes, err := clashes(opts.From, opts.Into)
	if err != nil {
		return err
	}
	label := opts.Label
	if label == "" {
		label = "adopt"
	}
	now := time.Now()
	backupDir := paths.BackupDir(label, now)
	report(opts.Out, clashes, backupDir)
	if opts.DryRun {
		fmt.Fprintln(opts.Out, "dry-run: no files were changed")
		return nil
	}

	if err := backupClashes(opts.Into, clashes, label, now); err != nil {
		return err
	}
	if err := installCheckout(opts); err != nil {
		return err
	}

	st := State{Mode: "adopt", Date: now, Commit: commitHash(opts.Into), BackupDir: backupDir, ClashCount: len(clashes)}
	if err := Record(st); err != nil {
		return fmt.Errorf("adopted into %s but not recorded: %w", opts.Into, err)
	}
	audit.Log(audit.SetupAdopt, opts.Into, fmt.Sprintf("clashes=%d", len(clashes)))
	fmt.Fprintf(opts.Out, "\nadopted into %s\nnext:\n  make -C %s install\n  claude-code configure --check\n", opts.Into, opts.Into)
	return nil
}

// adoptPreflight refuses an Into that is already a checkout and a From that is not a clone.
func adoptPreflight(opts adoptOpts) error {
	if _, err := os.Stat(filepath.Join(opts.Into, ".git")); err == nil {
		return errAlreadyCheckout
	}
	if _, err := os.Stat(filepath.Join(opts.From, ".git")); err != nil {
		return fmt.Errorf("%s: %w", opts.From, errNotAClone)
	}
	return nil
}

// backupClashes backs up every clashing path under into; no clashes means no backup.
func backupClashes(into string, clashes []clash, label string, now time.Time) error {
	if len(clashes) == 0 {
		return nil
	}
	relPaths := make([]string, len(clashes))
	for i, c := range clashes {
		relPaths[i] = c.Path
	}
	return backup(into, relPaths, label, now)
}

// installCheckout moves the staged .git into Into and drops the stage.
func installCheckout(opts adoptOpts) error {
	if err := os.MkdirAll(opts.Into, 0o755); err != nil {
		return err
	}
	if err := moveGitAndCheckout(opts.From, opts.Into); err != nil {
		return err
	}
	if err := os.RemoveAll(opts.From); err != nil { // the checkout is in; a leftover stage must not skip the record
		fmt.Fprintf(opts.Out, "warning: could not remove staged clone %s: %v\n", opts.From, err)
	}
	return nil
}

func commitHash(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:noctx // one-shot local git call; no caller ctx
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// checkoutTimeout bounds the restore so a hung git fails into the rollback instead of blocking with .git moved.
const checkoutTimeout = 5 * time.Minute

// moveGitAndCheckout moves from/.git into into and checks out; on failure .git moves back so a retry works.
func moveGitAndCheckout(from, into string) error {
	if err := os.Rename(filepath.Join(from, ".git"), filepath.Join(into, ".git")); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkoutTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", into, "checkout", "--", ".").CombinedOutput()
	if err == nil {
		return nil
	}
	err = fmt.Errorf("git checkout: %w: %s", err, strings.TrimSpace(string(out)))
	if rbErr := os.Rename(filepath.Join(into, ".git"), filepath.Join(from, ".git")); rbErr != nil {
		err = errors.Join(err, rbErr)
	}
	return err
}

// State is the record of the last adopt (state/harness/setup.json); the doctor row reads it back.
type State struct {
	Schema     int       `json:"schema"`
	Mode       string    `json:"mode"`
	Date       time.Time `json:"date"`
	Commit     string    `json:"commit"`
	BackupDir  string    `json:"backupDir"`
	ClashCount int       `json:"clashCount"`
}

func stateFile() string { return filepath.Join(paths.StateDir(), "setup.json") }

// Record persists st, overwriting any prior record.
func Record(st State) error {
	st.Schema = stateSchema
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(stateFile(), data, 0o600)
}

// Read returns the last recorded state; ok is false when none exists or it is unreadable.
func Read() (State, bool) {
	var st State
	data, err := os.ReadFile(stateFile())
	if err != nil {
		return st, false
	}
	if _, err := schemas.Decode(setupFormat, data, &st); err != nil {
		return st, false
	}
	return st, true
}

// stateSchema versions setup-state.
const stateSchema = 1

var setupFormat = schemas.Register(schemas.Format{
	Name: "setup-state", Owner: "internal/setup", Kind: schemas.Record, Version: stateSchema,
	Glob:    func() string { return stateFile() },
	New:     func() any { return new(State) },
	Keep:    "last adopt record",
	Migrate: schemas.SameShape,
})

// backup copies each of relPaths (dir-relative, slash-separated) into paths.BackupDir(label, now),
// preserving the file mode.
func backup(dir string, relPaths []string, label string, now time.Time) error {
	dest := paths.BackupDir(label, now)
	for _, rel := range relPaths {
		if err := copyFile(filepath.Join(dir, rel), filepath.Join(dest, rel)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	if li, err := os.Lstat(src); err == nil && li.Mode()&os.ModeSymlink != 0 {
		return copyLink(src, dst) // a symlink (even a dangling one) is backed up as a link, never followed
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return copyTree(src, dst)
	}
	if !fi.Mode().IsRegular() {
		return nil // FIFOs, sockets and devices are skipped: reading them can block forever
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(dst, data, fi.Mode())
}

// copyTree backs up a directory's files and links under dst; symlinks inside are copied as links, never followed.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

func copyLink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, dst)
}

// report prints one line per clash: path, same|differs, and where it lands under backupDir.
func report(w io.Writer, clashes []clash, backupDir string) {
	fmt.Fprintf(w, "%-50s %-8s %s\n", "PATH", "STATE", "BACKUP")
	for _, c := range clashes {
		state := "same"
		if c.Differs {
			state = "differs"
		}
		fmt.Fprintf(w, "%-50s %-8s %s\n", c.Path, state, filepath.Join(backupDir, c.Path))
	}
}
