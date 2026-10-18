package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StaleAfter is the age threshold past which an entry's last-activity evidence marks it Stale.
const StaleAfter = 14 * 24 * time.Hour

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
	Problems       []string
}

// List returns every worktree under repo.Root: the union of git's own registration and the immediate
// directories on disk, each inspected and classified. Entries outside repo.Root (including the main
// checkout's own registration record) are out of scope for this lifecycle and excluded.
func List(repo *Repo) ([]Entry, error) {
	registered, err := listPorcelain(repo)
	if err != nil {
		return nil, err
	}
	entries := unionFilesystem(repo, registered)
	target, targetErr := currentBranch(repo.Main)
	now := time.Now()
	for i := range entries {
		Classify(&entries[i])
		if entries[i].Exists {
			Inspect(repo, &entries[i], target, targetErr, now)
		}
	}
	return entries, nil
}

// listPorcelain parses `git worktree list --porcelain -z`, scoped to records under repo.Root; the
// main checkout's own record and any worktree registered outside repo.Root are not this lifecycle's.
func listPorcelain(repo *Repo) ([]Entry, error) {
	out, err := exec.Command("git", "-C", repo.Main, "worktree", "list", "--porcelain", "-z").Output()
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, rec := range splitRecords(string(out)) {
		e := parseRecord(rec)
		if e.Path == "" {
			continue
		}
		if p, err := canonical(e.Path); err == nil {
			e.Path = p
		}
		if e.Path == repo.Main {
			continue // the main checkout itself is not a lifecycle target
		}
		if rel, err := filepath.Rel(repo.Root, e.Path); err != nil || strings.HasPrefix(rel, "..") {
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
	for _, rec := range strings.Split(out, "\x00\x00") {
		if rec = strings.Trim(rec, "\x00"); rec != "" {
			recs = append(recs, rec)
		}
	}
	return recs
}

// parseRecord reads one porcelain record's NUL-separated "key value" lines.
func parseRecord(rec string) Entry {
	var e Entry
	for _, line := range strings.Split(rec, "\x00") {
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
func unionFilesystem(repo *Repo, registered []Entry) []Entry {
	byPath := map[string]int{}
	for i, e := range registered {
		byPath[e.Path] = i
		if _, err := os.Stat(e.Path); err == nil {
			registered[i].Exists = true
		}
	}
	dirs, _ := os.ReadDir(repo.Root)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(repo.Root, d.Name())
		if canon, err := canonical(path); err == nil {
			path = canon
		}
		if _, ok := byPath[path]; ok {
			continue
		}
		registered = append(registered, Entry{Path: path, Exists: true})
	}
	return registered
}

// Classify derives Orphaned from a registration/filesystem mismatch; Prunable/Locked already come
// straight from git's own porcelain output and need no further derivation here.
func Classify(e *Entry) {
	e.Orphaned = e.Registered != e.Exists
}

// Inspect fills Dirty, Merged, Stale and activity evidence on e, which must already exist on disk.
// A failed git call appends to Problems and leaves the affected field at its fail-safe default:
// Dirty=true (never silently "clean"), Merged=false (never silently eligible for removal).
func Inspect(repo *Repo, e *Entry, target string, targetErr error, now time.Time) {
	e.Dirty = true
	out, err := exec.Command("git", "-C", e.Path, "status", "--porcelain", "--ignore-submodules=none").Output()
	switch {
	case err != nil:
		e.Problems = append(e.Problems, "dirty check failed: "+err.Error())
	default:
		e.Dirty = len(strings.TrimSpace(string(out))) > 0
	}

	switch {
	case targetErr != nil:
		e.Problems = append(e.Problems, "merge target unresolved: "+targetErr.Error())
	case e.Head == "":
		e.Problems = append(e.Problems, "worktree HEAD unknown")
	default:
		cmd := exec.Command("git", "-C", repo.Main, "merge-base", "--is-ancestor", e.Head, target)
		if runErr := cmd.Run(); runErr == nil {
			e.Merged = true
		} else if exitErr, ok := asExitErr(runErr); !ok || exitErr != 1 {
			e.Problems = append(e.Problems, "merge check failed: "+runErr.Error())
		} // exit status 1 from --is-ancestor is a clean "no", not a failure
	}

	gitDir, gdErr := exec.Command("git", "-C", e.Path, "rev-parse", "--path-format=absolute", "--git-dir").Output()
	if gdErr != nil {
		e.Problems = append(e.Problems, "git-dir lookup failed: "+gdErr.Error())
	} else {
		e.ActivityAt, e.ActivitySource = activity(strings.TrimSpace(string(gitDir)), e.Path)
	}
	e.Stale = !e.ActivityAt.IsZero() && now.Sub(e.ActivityAt) > StaleAfter
}

// activity picks the freshest available evidence of last use: the reflog, then the index, then the
// worktree directory's own mtime — each is a heuristic, named in ActivitySource for the caller.
func activity(gitDir, worktreePath string) (time.Time, string) {
	if fi, err := os.Stat(filepath.Join(gitDir, "logs", "HEAD")); err == nil {
		return fi.ModTime(), "reflog"
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "index")); err == nil {
		return fi.ModTime(), "index"
	}
	if fi, err := os.Stat(worktreePath); err == nil {
		return fi.ModTime(), "filesystem"
	}
	return time.Time{}, ""
}

// currentBranch is main's checked-out branch (the Phase 1 merge target); detached HEAD uses the OID.
func currentBranch(main string) (string, error) {
	if out, err := exec.Command("git", "-C", main, "symbolic-ref", "--short", "-q", "HEAD").Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	out, err := exec.Command("git", "-C", main, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// asExitErr extracts a process exit code, so Inspect can tell merge-base's clean "not an ancestor"
// (exit 1) apart from a real failure (anything else, including a non-ExitError like a missing binary).
func asExitErr(err error) (int, bool) {
	type exitCoder interface{ ExitCode() int }
	if ee, ok := err.(exitCoder); ok {
		return ee.ExitCode(), true
	}
	return 0, false
}
