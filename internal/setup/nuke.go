package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// errConfirmMismatch is returned when --apply's typed confirmation doesn't match Dir exactly.
var errConfirmMismatch = errors.New("confirmation did not match the target directory")

// errNotConfigDir refuses a nuke of the filesystem root, home or a parent of home: only a config dir goes.
var errNotConfigDir = errors.New("refusing: not a Claude config dir (filesystem root, home or a parent of home)")

// nukeFloor rejects dir when it is "/", the user's home, or an ancestor of home, whatever the caller typed.
func nukeFloor(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = paths.Canon(abs)
	if abs == string(filepath.Separator) {
		return errNotConfigDir
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	home = paths.Canon(home)
	if rel, err := filepath.Rel(abs, home); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errNotConfigDir
	}
	return nil
}

// entry is one top-level path under the directory being nuked.
type entry struct {
	Name  string
	Size  int64
	Class string // "tracked" | "untracked" | "ignored" | "no-git"
}

// nukePlan is what nuke would do to dir, without doing it.
type nukePlan struct {
	Dir          string
	Entries      []entry
	HasSecrets   bool
	SecretNames  []string
	VaultID      string
	Kid          string
	VaultWarning string   // set when the vault couldn't be read; it is still archived opaquely, never named
	UserContent  []string // untracked-user-content paths nuke archives before removing dir (+ the keep-memory set when kept)
}

// planNuke inspects dir and reports what a nuke would remove, archive, and never touch (secret values).
func planNuke(dir string, keepMemory bool) (nukePlan, error) {
	plan := nukePlan{Dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return plan, err
	}
	hasGit := isGitRepo(dir)
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		class := "no-git"
		if hasGit {
			class = classify(dir, e.Name())
		}
		plan.Entries = append(plan.Entries, entry{Name: e.Name(), Size: paths.DirSize(filepath.Join(dir, e.Name())), Class: class})
	}
	// An unreadable/corrupt vault must not block the nuke: it is still archived whole, as an opaque
	// file, by userContentPaths below — just without naming the secrets it may hold.
	if err := vault.ViewAt(filepath.Join(dir, "state", "vault.db"), func(tx *vault.Tx) error {
		plan.SecretNames = tx.Names(vault.Secrets)
		plan.HasSecrets = len(plan.SecretNames) > 0
		plan.VaultID, _ = tx.Get(vault.Meta, "vault_id")
		plan.Kid, _ = tx.Get(vault.Meta, "kid")
		return nil
	}); err != nil {
		plan.VaultWarning = err.Error()
	}
	userContent, err := userContentPaths(dir, hasGit)
	if err != nil {
		return plan, fmt.Errorf("scan user content: %w", err)
	}
	plan.UserContent = userContent
	if keepMemory {
		kept, err := keepMemoryPaths(dir)
		if err != nil {
			return plan, fmt.Errorf("scan memory paths: %w", err)
		}
		plan.UserContent = append(plan.UserContent, kept...)
	}
	return plan, nil
}

// nukeOpts configures nuke.
type nukeOpts struct {
	Dir        string
	Apply      bool
	Confirm    string
	KeepMemory bool
	Out        io.Writer
}

// nuke previews by default; confirmed Apply archives user content and the sealed vault, then removes opts.Dir.
func nuke(opts nukeOpts) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if err := nukeFloor(opts.Dir); err != nil {
		return err
	}
	plan, err := planNuke(opts.Dir, opts.KeepMemory)
	if err != nil {
		return err
	}
	printPlan(opts.Out, plan)
	if !opts.Apply {
		return nil
	}
	if opts.Confirm != opts.Dir {
		return errConfirmMismatch
	}
	if !opts.KeepMemory {
		return archiveAndRemove(opts, plan)
	}
	// The bank lock spans re-plan, archive and removal: a concurrent memory write is never deleted unarchived.
	return memory.WithBankLock(0, func() error {
		fresh, err := planNuke(opts.Dir, true)
		if err != nil {
			return err
		}
		return archiveAndRemove(opts, fresh)
	})
}

func archiveAndRemove(opts nukeOpts, plan nukePlan) error {
	tarPath, err := archiveUserContent(plan, opts.KeepMemory)
	if err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "archived user content to %s\n", tarPath)
	// The audit log lives inside Dir and is about to be removed with it: log to stderr instead.
	fmt.Fprintf(os.Stderr, "claude-code: setup_nuke %s apply\n", opts.Dir)
	return os.RemoveAll(opts.Dir)
}

// createBackupFile opens a fresh claude-nuke-<date>.tgz under home with O_EXCL: two nukes in the
// same second get distinct files instead of one silently truncating the other's backup.
func createBackupFile(home string) (string, *os.File, error) {
	base := filepath.Join(home, "claude-nuke-"+time.Now().Format("20060102-150405"))
	for i := 0; ; i++ {
		dest := base + ".tgz"
		if i > 0 {
			dest = fmt.Sprintf("%s-%d.tgz", base, i)
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return dest, f, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
}

func printPlan(w io.Writer, plan nukePlan) {
	fmt.Fprintf(w, "nuke plan for %s\n", plan.Dir)
	for _, e := range plan.Entries {
		fmt.Fprintf(w, "  %-30s %12d bytes  %s\n", e.Name, e.Size, e.Class)
	}
	if plan.VaultWarning != "" {
		fmt.Fprintf(w, "vault.db unreadable (%s); archiving it opaquely, secret names unknown\n", plan.VaultWarning)
	}
	if plan.HasSecrets {
		fmt.Fprintf(w, "secrets present (names only, never values): %s\n", strings.Join(plan.SecretNames, ", "))
	}
	if len(plan.UserContent) > 0 {
		fmt.Fprintf(w, "untracked user content to archive (%d files):\n", len(plan.UserContent))
		for _, p := range plan.UserContent {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// classify reports whether name (a top-level entry of a git-tracked dir) is tracked, ignored, or
// neither. A directory counts as "tracked" if any file beneath it is.
func classify(repo, name string) string {
	if out, err := exec.Command("git", "-C", repo, "ls-files", "--", name).Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 { //nolint:noctx // one-shot local git call; no caller ctx
		return "tracked"
	}
	if err := exec.Command("git", "-C", repo, "check-ignore", "-q", "--", name).Run(); err == nil { //nolint:noctx // one-shot local git call; no caller ctx
		return "ignored"
	}
	return "untracked"
}

// userContentPaths lists the vault, settings.json, local settings, and untracked repo files nuke archives.
func userContentPaths(dir string, hasGit bool) ([]string, error) {
	var out []string
	for _, rel := range []string{"state/vault.db", "settings.json", "settings.local.json"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			out = append(out, rel)
		}
	}
	if !hasGit {
		return out, nil
	}
	for _, sub := range []string{"agents", "commands", "skills", "rules"} {
		rels, err := untrackedUnder(dir, sub)
		if err != nil {
			return nil, err
		}
		out = append(out, rels...)
	}
	return out, nil
}

// untrackedUnder lists the untracked files under repo/sub. A scan error (e.g. an unreadable subdir
// blocking `git ls-files`) must not be mistaken for "nothing untracked": it propagates instead.
func untrackedUnder(repo, sub string) ([]string, error) {
	fi, err := os.Stat(filepath.Join(repo, sub))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !fi.IsDir() {
		return nil, nil
	}
	out, err := exec.Command("git", "-C", repo, "ls-files", "-z", "--others", "--exclude-standard", "--", sub).Output() //nolint:noctx // one-shot local git call; no caller ctx
	if err != nil {
		return nil, fmt.Errorf("list untracked %s: %w", sub, err)
	}
	return splitNUL(out), nil
}

// archiveUserContent writes a tarball of plan.UserContent (+ a manifest naming secrets by key only,
// never value) to ~/claude-nuke-<date>.tgz; a failure removes the truncated tarball so none poses as a backup.
func archiveUserContent(plan nukePlan, keepMemory bool) (dest string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dest, f, err := createBackupFile(home)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(dest)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err = tarPlan(tw, plan, keepMemory); err != nil {
		return dest, err
	}
	// Explicit, ordered closes: each flushes buffered data, and a failed flush must fail the nuke.
	if err = tw.Close(); err != nil {
		return dest, err
	}
	if err = gz.Close(); err != nil {
		return dest, err
	}
	if err = f.Sync(); err != nil {
		return dest, err
	}
	if err = f.Close(); err != nil {
		return dest, err
	}
	return dest, nil
}

// tarPlan writes the manifest then plan.UserContent; with keepMemory the caller (nuke) holds the bank lock.
func tarPlan(tw *tar.Writer, plan nukePlan, keepMemory bool) error {
	manifest := struct {
		Date        time.Time `json:"date"`
		Dir         string    `json:"dir"`
		SecretNames []string  `json:"secretNames"`
		VaultID     string    `json:"vaultId"`
		Kid         string    `json:"kid"`
		KeepMemory  bool      `json:"keepMemory"`
	}{
		Date: time.Now().UTC(), Dir: plan.Dir, SecretNames: plan.SecretNames,
		VaultID: plan.VaultID, Kid: plan.Kid, KeepMemory: keepMemory,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := tarWriteBytes(tw, "manifest.json", data); err != nil {
		return err
	}
	for _, rel := range plan.UserContent {
		if rel == "state/vault.db" {
			var buf bytes.Buffer
			// A corrupt vault.db fails the consistent bbolt snapshot: archive it as a raw byte
			// copy instead (not point-in-time-safe, but still backed up rather than lost).
			if err := vault.SnapshotTo(filepath.Join(plan.Dir, rel), &buf); err != nil {
				if err := tarWriteFile(tw, plan.Dir, rel); err != nil {
					return err
				}
				continue
			}
			if err := tarWriteBytes(tw, rel, buf.Bytes()); err != nil {
				return err
			}
			continue
		}
		if err := tarWriteFile(tw, plan.Dir, rel); err != nil {
			return err
		}
	}
	return nil
}

func tarWriteBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func tarWriteFile(tw *tar.Writer, dir, rel string) error {
	// Untracked symlinks (dir links, dangling links) are skipped like regularFilesUnder does: the target lives elsewhere.
	if fi, err := os.Lstat(filepath.Join(dir, rel)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return err
	}
	return tarWriteBytes(tw, rel, data)
}

// keepMemoryPaths lists every regular file under memory/ (incl. .git, so per-bank knowledge/ rides along),
// plans/, an unmigrated legacy docs/memory/bank/, a legacy pre-44aab60 docs/knowledge/, and real
// projects/*/memory dirs, dir-relative. A symlinked native dir is the store's own duplicate: skipped, never followed.
// A walk error (e.g. an unreadable subdir) propagates instead of silently truncating the archive plan.
// Migration: remove after 2026-11-13 once every machine has run a post-2026-10-26 build.
func keepMemoryPaths(dir string) ([]string, error) {
	roots, err := memoryRoots(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, root := range roots {
		files, err := regularFilesUnder(dir, root)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}

// memoryRoots returns the existing real (non-symlink) directories under dir that keepMemoryPaths archives.
// An unreadable projects/ dir is an error: a root dropped from the plan would be deleted unarchived.
func memoryRoots(dir string) ([]string, error) {
	candidates := []string{"memory", filepath.Join("docs", "memory", "bank"), "plans", filepath.Join("docs", "knowledge")} // docs/knowledge is the pre-44aab60 knowledge location
	projects, err := projectMemoryRels(dir)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, projects...)
	var roots []string
	for _, rel := range candidates {
		if fi, err := os.Lstat(filepath.Join(dir, rel)); err == nil && fi.IsDir() {
			roots = append(roots, filepath.Join(dir, rel))
		}
	}
	return roots, nil
}

// projectMemoryRels lists projects/<slug>/memory for every entry under dir/projects; a missing projects/ is none.
func projectMemoryRels(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "projects"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan projects: %w", err)
	}
	var rels []string
	for _, e := range entries {
		rels = append(rels, filepath.Join("projects", e.Name(), "memory"))
	}
	return rels, nil
}

// regularFilesUnder lists the regular files under root as slash paths relative to dir; symlinks are skipped.
func regularFilesUnder(dir, root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil // symlinks are never read: tarWriteFile would follow an escape link
		}
		if rel, relErr := filepath.Rel(dir, p); relErr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return out, nil
}
