package setup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/providers"
)

// ErrConfirmMismatch is returned when --apply's typed confirmation doesn't match Dir exactly.
var ErrConfirmMismatch = errors.New("confirmation did not match the target directory")

// ErrNotConfigDir refuses a nuke of the filesystem root or the home directory: only a config dir goes.
var ErrNotConfigDir = errors.New("refusing: not a Claude config dir (filesystem root or home)")

// nukeFloor rejects dir when it is "/" or the user's home, whatever the caller typed.
func nukeFloor(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if abs == string(filepath.Separator) || (home != "" && abs == filepath.Clean(home)) {
		return ErrNotConfigDir
	}
	return nil
}

// Entry is one top-level path under the directory being nuked.
type Entry struct {
	Name  string
	Size  int64
	Class string // "tracked" | "untracked" | "ignored" | "no-git"
}

// Plan is what Nuke would do to dir, without doing it.
type NukePlan struct {
	Dir         string
	Entries     []Entry
	HasSecrets  bool
	SecretNames []string
	UserContent []string // untracked-user-content paths Nuke archives before removing dir (+ the keep-memory set when kept)
}

// Plan inspects dir and reports what a nuke would remove, archive, and never touch (secret values).
func Plan(dir string, keepMemory bool) (NukePlan, error) {
	plan := NukePlan{Dir: dir}
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
		plan.Entries = append(plan.Entries, Entry{Name: e.Name(), Size: dirSize(filepath.Join(dir, e.Name())), Class: class})
	}
	secretsFile := filepath.Join(dir, "env.d", "secrets.env")
	if _, err := os.Stat(secretsFile); err == nil {
		plan.HasSecrets = true
		for k := range providers.ReadEnvFile(secretsFile) {
			plan.SecretNames = append(plan.SecretNames, k)
		}
		sort.Strings(plan.SecretNames)
	}
	plan.UserContent = userContentPaths(dir, hasGit)
	if keepMemory {
		plan.UserContent = append(plan.UserContent, keepMemoryPaths(dir)...)
	}
	return plan, nil
}

// NukeOpts configures Nuke.
type NukeOpts struct {
	Dir        string
	Apply      bool
	Confirm    string
	KeepMemory bool
	Out        io.Writer
}

// Nuke prints PlanFor(opts.Dir); without Apply it stops there. With Apply it requires Confirm to
// equal opts.Dir exactly, archives the untracked-user-content set (never env.d/secrets.env) to
// ~/claude-nuke-<date>.tgz, then removes opts.Dir.
func Nuke(opts NukeOpts) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if err := nukeFloor(opts.Dir); err != nil {
		return err
	}
	plan, err := Plan(opts.Dir, opts.KeepMemory)
	if err != nil {
		return err
	}
	printPlan(opts.Out, plan)
	if !opts.Apply {
		return nil
	}
	if opts.Confirm != opts.Dir {
		return ErrConfirmMismatch
	}
	tarPath, err := archiveUserContent(plan, opts.KeepMemory)
	if err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "archived user content to %s\n", tarPath)
	// The audit log lives inside Dir and is about to be removed with it: log to stderr instead.
	fmt.Fprintf(os.Stderr, "claude-code: %s %s apply\n", audit.SetupNuke, opts.Dir)
	return os.RemoveAll(opts.Dir)
}

func printPlan(w io.Writer, plan NukePlan) {
	fmt.Fprintf(w, "nuke plan for %s\n", plan.Dir)
	for _, e := range plan.Entries {
		fmt.Fprintf(w, "  %-30s %12d bytes  %s\n", e.Name, e.Size, e.Class)
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
	if out, err := exec.Command("git", "-C", repo, "ls-files", "--", name).Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		return "tracked"
	}
	if err := exec.Command("git", "-C", repo, "check-ignore", "-q", "--", name).Run(); err == nil {
		return "ignored"
	}
	return "untracked"
}

func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// userContentPaths is the untracked-user-content set: hand-authored config plus any untracked file
// under a few tracked trees (a project's own agent/command/skill). Never env.d/secrets.env.
func userContentPaths(dir string, hasGit bool) []string {
	var out []string
	for _, rel := range []string{"env.d/providers.json", "settings.local.json", "env.d/provider.env"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			out = append(out, rel)
		}
	}
	if !hasGit {
		return out
	}
	for _, sub := range []string{"agents", "commands", "skills", "rules"} {
		out = append(out, untrackedUnder(dir, sub)...)
	}
	return out
}

func untrackedUnder(repo, sub string) []string {
	if fi, err := os.Stat(filepath.Join(repo, sub)); err != nil || !fi.IsDir() {
		return nil
	}
	out, err := exec.Command("git", "-C", repo, "ls-files", "--others", "--exclude-standard", "--", sub).Output()
	if err != nil {
		return nil
	}
	var rels []string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l != "" {
			rels = append(rels, l)
		}
	}
	return rels
}

// archiveUserContent writes a tarball of plan.UserContent (+ a manifest naming secrets by key only,
// never value) to ~/claude-nuke-<date>.tgz; a failure removes the truncated tarball so none poses as a backup.
func archiveUserContent(plan NukePlan, keepMemory bool) (dest string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dest = filepath.Join(home, "claude-nuke-"+time.Now().Format("20060102-150405")+".tgz")
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(dest)
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
	if err = f.Close(); err != nil {
		return dest, err
	}
	return dest, nil
}

// tarPlan writes the manifest then plan.UserContent. With keepMemory the plan carries memory/ (incl. .git),
// so the file reads run under the bank lock: a concurrent settle/commit must not land a torn .git in the backup.
func tarPlan(tw *tar.Writer, plan NukePlan, keepMemory bool) error {
	manifest := struct {
		Date        time.Time `json:"date"`
		Dir         string    `json:"dir"`
		SecretNames []string  `json:"secretNames"`
		KeepMemory  bool      `json:"keepMemory"`
	}{Date: time.Now().UTC(), Dir: plan.Dir, SecretNames: plan.SecretNames, KeepMemory: keepMemory}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := tarWriteBytes(tw, "manifest.json", data); err != nil {
		return err
	}
	write := func() error {
		for _, rel := range plan.UserContent {
			if err := tarWriteFile(tw, plan.Dir, rel); err != nil {
				return err
			}
		}
		return nil
	}
	if keepMemory {
		return memory.WithBankLock(0, write)
	}
	return write()
}

func tarWriteBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func tarWriteFile(tw *tar.Writer, dir, rel string) error {
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return err
	}
	return tarWriteBytes(tw, rel, data)
}

// keepMemoryPaths lists every regular file under memory/ (incl. .git), docs/knowledge/, plans/, an unmigrated legacy
// docs/memory/bank/ and real projects/*/memory dirs, dir-relative. A symlinked native dir is the store's own duplicate: skipped, never followed.
func keepMemoryPaths(dir string) []string {
	candidates := []string{"memory", filepath.Join("docs", "memory", "bank"), filepath.Join("docs", "knowledge"), "plans"}
	if matches, err := filepath.Glob(filepath.Join(dir, "projects", "*", "memory")); err == nil {
		for _, m := range matches {
			if rel, err := filepath.Rel(dir, m); err == nil {
				candidates = append(candidates, rel)
			}
		}
	}
	var roots []string
	for _, rel := range candidates {
		if fi, err := os.Lstat(filepath.Join(dir, rel)); err == nil && fi.IsDir() {
			roots = append(roots, filepath.Join(dir, rel))
		}
	}
	var out []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil // symlinks are never read: tarWriteFile would follow an escape link
			}
			if rel, relErr := filepath.Rel(dir, p); relErr == nil {
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	return out
}
