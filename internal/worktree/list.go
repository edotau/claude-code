package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// staleAfter is the age threshold past which an entry's last-activity evidence marks it Stale.
const staleAfter = 14 * 24 * time.Hour

// Entry is one worktree; a failed inspection appends to Problems and leaves the fail-safe default (Dirty, not Merged).
type Entry struct {
	Path       string
	Branch     string
	Head       string
	Registered bool
	Exists     bool
	Locked     bool
	Dirty      bool
	Merged     bool
	Stale      bool
	Orphaned   bool
	// git itself reports stale administrative registration
	Prunable   bool
	ActivityAt time.Time
	// "reflog" | "index" | "filesystem" | "" (no evidence found)
	ActivitySource string
	// live refusal reason from in-use, lock or operation checks ("" = free)
	Busy     string
	Problems []string
	// commits the branch has that the merge target lacks, and the target has that it lacks
	Ahead, Behind int
	// other lanes (by name) and "main" (its uncommitted edits) mapped to the changed files shared with this lane
	Overlaps map[string][]string `json:",omitempty"`
	// dest's git-dir when the caller already resolved it
	gitDir string
}

// List returns every worktree under repo.Root (git registration plus directories on disk), each inspected.
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
	var in func(string) ([]Proc, error)
	for i := range entries {
		entries[i].Orphaned = entries[i].Registered != entries[i].Exists
		if entries[i].Exists {
			if in == nil {
				in = firstLook()
			}
			inspect(repo, &entries[i], target, targetErr, now)
			_, entries[i].Busy = cleanBusyReason(entries[i].Path, in)
		}
	}
	if targetErr == nil {
		fillDivergence(repo, entries, target)
	}
	return entries, nil
}

// fillDivergence sets Ahead/Behind per branch and Overlaps: files two lanes changed, or a lane and main's dirty edits.
func fillDivergence(repo *Repo, entries []Entry, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	files := map[string]map[string]bool{}
	if out, err := gitCmd(ctx, "-C", repo.Main, "status", "--porcelain", "-z", "--untracked-files=no").Output(); err == nil {
		files["main"] = statusFiles(string(out))
	}
	for i := range entries {
		e := &entries[i]
		if !e.Exists || e.Branch == "" {
			continue
		}
		var counts, changed []byte
		var countErr, diffErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			counts, countErr = gitCmd(ctx, "-C", repo.Main, "rev-list", "--left-right", "--count", target+"..."+e.Branch).Output()
		})
		wg.Go(func() {
			changed, diffErr = gitCmd(ctx, "-C", repo.Main, "diff", "--name-only", "-z", target+"..."+e.Branch).Output()
		})
		wg.Wait()
		if countErr == nil {
			fmt.Sscanf(string(counts), "%d %d", &e.Behind, &e.Ahead)
		}
		if diffErr == nil {
			files[laneLabel(entries, e.Path)] = nameSet(strings.Split(string(changed), "\x00"))
		}
	}
	for i := range entries {
		name := laneLabel(entries, entries[i].Path)
		for other, set := range files {
			if other == name {
				continue
			}
			if shared := sharedFiles(files[name], set); len(shared) > 0 {
				if entries[i].Overlaps == nil {
					entries[i].Overlaps = map[string][]string{}
				}
				entries[i].Overlaps[other] = shared
			}
		}
	}
}

func laneLabel(entries []Entry, path string) string {
	name := filepath.Base(path)
	for _, e := range entries {
		if name == "main" || (e.Path != path && filepath.Base(e.Path) == name) {
			return path
		}
	}
	return name
}

// statusFiles reads the paths out of `git status --porcelain -z` (a rename record carries a second path).
func statusFiles(out string) map[string]bool {
	set := map[string]bool{}
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		if len(recs[i]) > 3 {
			set[recs[i][3:]] = true
			if strings.ContainsAny(recs[i][:2], "RC") && i+1 < len(recs) {
				i++
				set[recs[i]] = true
			}
		}
	}
	return set
}

func nameSet(names []string) map[string]bool {
	set := map[string]bool{}
	for _, n := range names {
		if n != "" {
			set[n] = true
		}
	}
	return set
}

// sharedFiles is the sorted intersection of two file sets.
func sharedFiles(a, b map[string]bool) []string {
	var shared []string
	for f := range a {
		if b[f] {
			shared = append(shared, f)
		}
	}
	slices.Sort(shared)
	return shared
}

// listPorcelain parses `git worktree list --porcelain -z`, keeping only records under repo.Root.
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
			continue
		}
		if !underRoots(repo, e.Path) {
			// registered elsewhere on disk, outside the convention this package governs
			continue
		}
		e.Registered = true
		entries = append(entries, e)
	}
	return entries, nil
}

// splitRecords breaks porcelain -z output into per-worktree blocks (a doubled NUL ends one).
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

// unionFilesystem adds an unregistered entry for each directory under repo.Root git did not list, and stats Exists.
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
	for _, root := range repoRoots(repo) {
		dirs, err := worktreeDirNames(root)
		if err != nil {
			return nil, err
		}
		for _, name := range dirs {
			path := paths.Canon(filepath.Join(root, name))
			if _, ok := byPath[path]; ok {
				continue
			}
			registered = append(registered, Entry{Path: path, Exists: true})
		}
	}
	return registered, nil
}

// repoRoots lists the roots to scan: Root, then Legacy when set.
func repoRoots(repo *Repo) []string {
	if repo.Legacy == "" {
		return []string{repo.Root}
	}
	return []string{repo.Root, repo.Legacy}
}

// underRoots reports whether path sits at or below one of repo's roots.
func underRoots(repo *Repo, path string) bool {
	for _, root := range repoRoots(repo) {
		if rel, err := filepath.Rel(paths.Canon(root), path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// inspect fills Dirty, Merged, Stale and activity on e; a failed git call goes to Problems with the fail-safe default.
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

// activity picks the newer of the reflog and index as last-use evidence, else the directory mtime (named in source).
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

// asExitErr extracts a process exit code, so callers tell git's clean exit 1 ("no") from a real failure.
func asExitErr(err error) (int, bool) {
	type exitCoder interface{ ExitCode() int }
	if ee, ok := err.(exitCoder); ok {
		return ee.ExitCode(), true
	}
	return 0, false
}

// EligibleForClean mirrors the bar Clean rechecks live (cleanBusyReason, cleanInspectReason); list never acts.
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
	var names []string
	for _, root := range repoRoots(repo) {
		n, _ := worktreeDirNames(root)
		names = append(names, n...)
	}
	return names
}
