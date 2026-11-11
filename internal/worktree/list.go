package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// staleAfter is the age threshold past which an entry's last-activity evidence marks it Stale.
const staleAfter = 14 * 24 * time.Hour

// Entry is one worktree, from git's registration, the filesystem, or both. Flags overlap (an entry
// can be Dirty and Stale at once); a failed inspection appends to Problems and leaves the affected
// boolean at its safe default (Dirty=true, Merged=false) rather than claiming "clean".
type Entry struct {
	Path           string
	Branch         string // short branch name, "" when detached
	Head           string // full OID, "" when unknown
	Registered     bool   // present in `git worktree list --porcelain`
	Exists         bool   // the directory is actually on disk
	Locked         bool
	Dirty          bool
	Merged         bool
	Stale          bool
	Orphaned       bool // registration and filesystem disagree
	Prunable       bool // git itself reports stale administrative registration
	ActivityAt     time.Time
	ActivitySource string // "reflog" | "index" | "filesystem" | "" (no evidence found)
	Busy           string // live refusal reason from in-use, lock or operation checks ("" = free)
	Problems       []string
	gitDir         string // dest's git-dir when the caller already resolved it
}

// List returns every worktree under repo.Root: the union of git's own registration and the immediate
// directories on disk, each inspected and classified. Entries outside repo.Root (including the main
// checkout's own registration record) are out of scope for this lifecycle and excluded.
func List(repo *Repo) ([]Entry, error) {
	registered, err := listPorcelain(repo)
	if err != nil {
		return nil, err
	}
	entries, err := unionFilesystem(repo, registered)
	if err != nil {
		return nil, err
	}
	target, targetErr := currentBranch(repo.Main)
	now := time.Now()
	for i := range entries {
		entries[i].Orphaned = entries[i].Registered != entries[i].Exists // Prunable/Locked come from git's porcelain
		if entries[i].Exists {
			inspect(repo, &entries[i], target, targetErr, now)
			_, entries[i].Busy = cleanBusyReason(entries[i].Path)
		}
	}
	return entries, nil
}

// listPorcelain parses `git worktree list --porcelain -z`, scoped to records under repo.Root; the
// main checkout's own record and any worktree registered outside repo.Root are not this lifecycle's.
func listPorcelain(repo *Repo) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := gitCmd(ctx, "-C", repo.Main, "worktree", "list", "--porcelain", "-z").Output()
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, rec := range splitRecords(string(out)) {
		e := parseRecord(rec)
		if e.Path == "" {
			continue
		}
		e.Path = paths.Canon(e.Path)
		if e.Path == repo.Main {
			continue // the main checkout itself is not a lifecycle target
		}
		if rel, err := filepath.Rel(repo.Root, e.Path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // registered elsewhere on disk, outside the convention this package governs
		}
		e.Registered = true
		entries = append(entries, e)
	}
	return entries, nil
}

// splitRecords breaks porcelain -z output into per-worktree blocks: fields are NUL-terminated, and
// an empty field (two consecutive NULs) marks the end of one record.
func splitRecords(out string) []string {
	var recs []string
	for rec := range strings.SplitSeq(out, "\x00\x00") {
		if rec = strings.Trim(rec, "\x00"); rec != "" {
			recs = append(recs, rec)
		}
	}
	return recs
}

// parseRecord reads one porcelain record's NUL-separated "key value" lines.
func parseRecord(rec string) Entry {
	var e Entry
	for line := range strings.SplitSeq(rec, "\x00") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			e.Path = val
		case "HEAD":
			e.Head = val
		case "branch":
			e.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "locked":
			e.Locked = true
		case "prunable":
			e.Prunable = true
		}
	}
	return e
}

// unionFilesystem adds an Exists=true, Registered=false entry for every immediate directory under
// repo.Root that listPorcelain didn't already find, and marks every registered entry's Exists by stat.
func unionFilesystem(repo *Repo, registered []Entry) ([]Entry, error) {
	byPath := map[string]int{}
	for i, e := range registered {
		byPath[e.Path] = i
		_, err := os.Stat(e.Path)
		switch {
		case err == nil:
			registered[i].Exists = true
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("stat registered worktree %s: %w", e.Path, err)
		}
	}
	dirs, err := worktreeDirNames(repo.Root)
	if err != nil {
		return nil, err
	}
	for _, name := range dirs {
		path := paths.Canon(filepath.Join(repo.Root, name))
		if _, ok := byPath[path]; ok {
			continue
		}
		registered = append(registered, Entry{Path: path, Exists: true})
	}
	return registered, nil
}

// inspect fills Dirty, Merged, Stale and activity evidence on e, which must already exist on disk.
// A failed git call appends to Problems and leaves the affected field at its fail-safe default:
// Dirty=true (never silently "clean"), Merged=false (never silently eligible for removal).
func inspect(repo *Repo, e *Entry, target string, targetErr error, now time.Time) {
	e.Dirty = true
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if dirty, err := isDirty(ctx, e.Path); err != nil {
		e.Problems = append(e.Problems, "dirty check failed: "+err.Error())
	} else {
		e.Dirty = dirty
	}

	switch {
	case targetErr != nil:
		e.Problems = append(e.Problems, "merge target unresolved: "+targetErr.Error())
	case e.Head == "":
		e.Problems = append(e.Problems, "worktree HEAD unknown")
	default:
		merged, runErr := isAncestor(ctx, repo.Main, e.Head, target)
		e.Merged = merged
		if runErr != nil {
			e.Problems = append(e.Problems, "merge check failed: "+runErr.Error())
		}
	}

	gitDir, gdErr := e.gitDir, error(nil)
	if gitDir == "" {
		gitDir, gdErr = gitDirFromPath(e.Path)
	}
	if gdErr != nil {
		e.Problems = append(e.Problems, "git-dir lookup failed: "+gdErr.Error())
	} else {
		e.ActivityAt, e.ActivitySource = activity(gitDir, e.Path)
	}
	e.Stale = !e.ActivityAt.IsZero() && now.Sub(e.ActivityAt) > staleAfter
}

// gitDirFromPath resolves a worktree's git-dir from <path>/.git: a directory, or a linked worktree's gitdir file.
func gitDirFromPath(path string) (string, error) {
	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return dotGit, nil
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s: no gitdir: line", dotGit)
	}
	dir := strings.TrimSpace(rest)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(path, dir)
	}
	return dir, nil
}

// activity picks the newer of the reflog and the index as last-use evidence, else the worktree directory's
// mtime; each is a heuristic, named in ActivitySource for the caller.
func activity(gitDir, worktreePath string) (time.Time, string) {
	var best time.Time
	source := ""
	for _, c := range []struct{ file, name string }{{"logs/HEAD", "reflog"}, {"index", "index"}} {
		if fi, err := os.Stat(filepath.Join(gitDir, filepath.FromSlash(c.file))); err == nil && fi.ModTime().After(best) {
			best, source = fi.ModTime(), c.name
		}
	}
	if source != "" {
		return best, source
	}
	if fi, err := os.Stat(worktreePath); err == nil {
		return fi.ModTime(), "filesystem"
	}
	return time.Time{}, ""
}

// probeTimeout bounds each read-only git/lsof/ps probe so a hung filesystem cannot block List, inspect or Clean.
var probeTimeout = 30 * time.Second

// currentBranch is main's checked-out branch (the merge target); detached HEAD uses the OID.
func currentBranch(main string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if out, err := gitCmd(ctx, "-C", main, "symbolic-ref", "--short", "-q", "HEAD").Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	out, err := gitCmd(ctx, "-C", main, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// asExitErr extracts a process exit code, so inspect can tell merge-base's clean "not an ancestor"
// (exit 1) apart from a real failure (anything else, including a non-ExitError like a missing binary).
func asExitErr(err error) (int, bool) {
	type exitCoder interface{ ExitCode() int }
	if ee, ok := err.(exitCoder); ok {
		return ee.ExitCode(), true
	}
	return 0, false
}

// EligibleForClean mirrors the bar Clean rechecks live (cleanBusyReason, cleanInspectReason);
// list only offers the command, never acts.
func (e Entry) EligibleForClean() bool {
	return e.Exists && !e.Orphaned && !e.Locked && !e.Dirty && e.Merged && e.Busy == "" && len(e.Problems) == 0
}

// StatusFlags lists the entry's state words for the text list; "clean" when none apply.
func (e Entry) StatusFlags() []string {
	var status []string
	if e.Dirty {
		status = append(status, "dirty")
	}
	if e.Merged {
		status = append(status, "merged")
	}
	if e.Stale {
		status = append(status, fmt.Sprintf("stale(%s)", e.ActivitySource))
	}
	if e.Orphaned {
		status = append(status, "orphaned")
	}
	if e.Prunable {
		status = append(status, "prunable")
	}
	if e.Locked {
		status = append(status, "locked")
	}
	if len(status) == 0 {
		status = append(status, "clean")
	}
	return status
}

// worktreeDirNames lists the directory names directly under root; a missing root is an empty list.
func worktreeDirNames(root string) ([]string, error) {
	ents, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Names lists the worktree dirs under cwd's repo root (~/worktrees/<repo>) — a plain listing, fast for Tab.
func Names(cwd string) []string {
	repo, err := ResolveRepo(cwd)
	if err != nil {
		return nil
	}
	names, _ := worktreeDirNames(repo.Root) // Tab completion: any error means no candidates
	return names
}
