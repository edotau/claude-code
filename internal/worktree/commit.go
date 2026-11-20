// The commit verb: one pathspec commit of named or claimed files, refused when a peer holds one.

package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
)

// CommitRequest is one `worktree commit` call; Source carries the caller's session, liveness probe and peer labels.
type CommitRequest struct {
	Dir, Message string
	Paths        []string
	Tasks        []string
	Plan         string
	DryRun       bool
	Source       ClaimSource
	Out          io.Writer
}

// CommitError is a refusal (Code 1) or a usage error (Code 2); nothing was changed.
type CommitError struct {
	Code int
	Msg  string
}

func (e *CommitError) Error() string { return e.Msg }

func refuse(format string, a ...any) error { return &CommitError{1, fmt.Sprintf(format, a...)} }
func usage(format string, a ...any) error  { return &CommitError{2, fmt.Sprintf(format, a...)} }

// commitTree is one read of the checkout: root, status entries and dirty paths (absolute, a rename source included).
type commitTree struct {
	top     string
	entries []proc.StatusEntry
	dirty   map[string]bool
}

func (t commitTree) abs(rel string) string { return filepath.Join(t.top, rel) }

// Commit commits the named files (else the caller's claimed dirty ones) in one pathspec commit and reports it.
func Commit(ctx context.Context, req CommitRequest) error {
	top, _ := paths.CheckoutOf(req.Dir)
	if top == "" {
		return usage("not inside a git work tree: %s", req.Dir)
	}
	entries, err := readOnlyStatus(ctx, paths.Real(top))
	if err != nil {
		return err
	}
	tree := commitTree{top: paths.Real(top), entries: entries, dirty: DirtyMap(paths.Real(top), entries)}
	if strings.TrimSpace(req.Message) == "" {
		return usage("give the message with -m")
	}
	dispatched := proc.InDispatch(ctx)
	set, err := commitSet(req, tree, dispatched)
	if err != nil {
		return err
	}
	msg, err := cleanMessage(req.Message, req.Out)
	if err != nil {
		return err
	}
	trailers, err := plans.TaskTrailers(tree.top, req.Plan, req.Tasks)
	if err != nil {
		return err
	}
	msg += trailers
	if err := checkHolders(ctx, req, tree, set, dispatched); err != nil {
		return err
	}
	untracked := printSet(ctx, req.Out, tree, set)
	if req.DryRun {
		return nil
	}
	res, err := runCommit(ctx, tree.top, msg, set, untracked)
	if err != nil {
		return err
	}
	fmt.Fprintln(req.Out, withoutShortstat(res))
	tickPlans(ctx, req.Out, tree.top, set)
	return nil
}

var commitShortstat = regexp.MustCompile(`^ [0-9]+ files? changed(?:, [0-9]+ (?:insertions?\(\+\)|deletions?\(-\)))*$`)

func withoutShortstat(out string) string {
	return strings.Join(slices.DeleteFunc(strings.Split(out, "\n"), commitShortstat.MatchString), "\n")
}

// tickPlans ticks the plans the new HEAD advances for every agent; a failure never fails the commit.
func tickPlans(ctx context.Context, out io.Writer, top string, set []string) {
	sha, err := proc.RunGit(ctx, top, "rev-parse", "HEAD")
	if err != nil {
		fmt.Fprintf(out, "plans: not ticked: %v\n", err)
		return
	}
	sha = strings.TrimSpace(sha)
	results, tied, err := plans.DeferTick(top, sha, set, os.Getenv(paths.SessionIDEnv))
	for _, r := range results {
		for _, n := range r.Lines(sha) {
			fmt.Fprintln(out, n)
		}
	}
	if n := plans.TieNote(tied); n != "" {
		fmt.Fprintln(out, n)
	}
	if errors.Is(err, plans.ErrNoTrunk) { // its text already says nothing ticked
		fmt.Fprintln(out, err)
	} else if err != nil {
		fmt.Fprintf(out, "plans: not ticked: %v\n", err)
	}
}

// readOnlyStatus is ReadDirty's status without git's opportunistic index refresh, which rewrites the index as 0644.
func readOnlyStatus(ctx context.Context, top string) ([]proc.StatusEntry, error) {
	raw, err := proc.RunGit(ctx, top, "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", top, err)
	}
	return proc.StatusEntries(raw), nil
}

// commitSet is the repo-relative dirty paths to commit: named ones, else the caller's own claimed ones.
func commitSet(req CommitRequest, t commitTree, dispatched bool) ([]string, error) {
	var want []string
	var err error
	if len(req.Paths) > 0 {
		if want, err = namedPaths(req.Dir, t.top, req.Paths); err != nil {
			return nil, err
		}
	} else if dispatched {
		return nil, usage("a dispatched leaf commits named files only: name your files " +
			"(claude-code worktree commit -m <msg> <files>)")
	} else if want, err = ownClaimed(req, t); err != nil {
		return nil, err
	}
	set := dirtyAmong(t.entries, want)
	if len(set) == 0 {
		return nil, refuse("nothing to commit: none of %s differs from HEAD", strings.Join(want, ", "))
	}
	return set, nil
}

// dirtyAmong is the sorted dirty paths among want, each rename's two paths together (git needs both).
func dirtyAmong(entries []proc.StatusEntry, want []string) []string {
	for _, e := range entries {
		if e.From != "" && (slices.Contains(want, e.Path) || slices.Contains(want, e.From)) {
			want = append(want, e.Path, e.From)
		}
	}
	var set []string
	for _, e := range entries {
		for _, p := range []string{e.Path, e.From} {
			if p != "" && slices.Contains(want, p) && !slices.Contains(set, p) {
				set = append(set, p)
			}
		}
	}
	slices.Sort(set)
	return set
}

// namedPaths resolves the arguments to repo-relative files; a directory, `.` or a glob matching one is a usage error.
func namedPaths(dir, top string, args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		p := core.AnchorPath(a, paths.Real(dir))
		matches := []string{p}
		if _, err := os.Lstat(p); err != nil && strings.ContainsAny(a, "*?[") {
			if g, _ := filepath.Glob(p); len(g) > 0 {
				matches = g
			}
		}
		for _, m := range matches {
			if st, err := os.Lstat(m); err == nil && st.IsDir() {
				return nil, usage("%s is a directory: name files, not directories", a)
			}
			rel, ok := inCheckout(top, m)
			if !ok {
				return nil, usage("%s is outside the checkout %s", a, top)
			}
			out = append(out, rel)
		}
	}
	return out, nil
}

// inCheckout is p relative to top, resolving a symlinked parent directory.
func inCheckout(top, p string) (string, bool) {
	if rel, ok := core.RelWithin(top, p); ok {
		return filepath.ToSlash(rel), true
	}
	rel, ok := core.RelWithin(top, filepath.Join(paths.Real(filepath.Dir(p)), filepath.Base(p)))
	return filepath.ToSlash(rel), ok
}

// ownClaimed is the dirty paths the main session claimed under top; any subagent's claim makes the pick ambiguous.
func ownClaimed(req CommitRequest, t commitTree) ([]string, error) {
	var out []string
	subagent := false
	own := req.Source.Owner
	_ = ScanClaims(req.Source.Alive, func(pid int, slug string, c Claim) {
		if slug != req.Source.Self && pid != own() {
			return
		}
		rel, ok := core.RelWithin(t.top, c.Path)
		if !ok || !t.dirty[c.Path] {
			return
		}
		subagent = subagent || c.Agent != ""
		if c.Agent != "" {
			return
		}
		if rel = filepath.ToSlash(rel); !slices.Contains(out, rel) {
			out = append(out, rel)
		}
	})
	switch {
	case subagent:
		return nil, usage("a subagent of this session also has claims: name your files " +
			"(claude-code worktree commit -m <msg> <files>)")
	case len(out) == 0:
		return nil, usage("no files to commit: name your files (claude-code worktree commit -m <msg> <files>)")
	}
	return out, nil
}

// checkHolders refuses a dirty path of the set that another live session claims; a CLI leaf is refused any claim.
func checkHolders(ctx context.Context, req CommitRequest, t commitTree, set []string, dispatched bool) error {
	src := req.Source
	src.Leaf = func(Claim) string { return "" } // outside a dispatch the worktree-claim-check hook judges this session
	if dispatched {
		src.Leaf = sessionHolder
		src.Dispatch = proc.Value(ctx, proc.DispatchIDEnv) // a claim this dispatch stamped is its own, whatever the session
	}
	claims, err := LiveClaims(src)
	if err != nil && len(claims) == 0 {
		return fmt.Errorf("could not read file claims: %w", err)
	}
	var claimed []string
	for _, rel := range set {
		if len(claims[t.abs(rel)]) > 0 {
			claimed = append(claimed, rel)
		}
	}
	res, errs := freshHitsAll(ctx, t, claimed, claims)
	var held []string
	for i, rel := range claimed {
		if errs[i] != nil {
			return errs[i]
		}
		for _, h := range res[i] {
			held = append(held, fmt.Sprintf("%s is claimed by %s (since %s)", rel, h.Who, time.Unix(h.TS, 0).Format("15:04")))
		}
	}
	if len(held) > 0 {
		return refuse("another live session or leaf has uncommitted edits in the set: %s; leave those files out or "+
			"hand them to their owner", strings.Join(held, "; "))
	}
	return nil
}

// freshHitsAll runs FreshHits per path on up to 4 workers, fully joined; results are indexed like paths.
func freshHitsAll(ctx context.Context, t commitTree, paths []string, claims map[string][]ClaimHit) ([][]ClaimHit, []error) {
	res := make([][]ClaimHit, len(paths))
	errs := make([]error, len(paths))
	query := func(i int) {
		p := t.abs(paths[i])
		res[i], errs[i] = FreshHits(ctx, t.top, p, t.dirty, claims[p], 0)
	}
	if len(paths) < 2 {
		for i := range paths {
			query(i)
		}
		return res, errs
	}
	var wg sync.WaitGroup
	next := make(chan int)
	for range min(4, len(paths)) {
		wg.Go(func() {
			for i := range next {
				query(i)
			}
		})
	}
	for i := range paths {
		next <- i
	}
	close(next)
	wg.Wait()
	return res, errs
}

// sessionHolder labels a claim of the caller's own session, which a CLI leaf may not commit.
func sessionHolder(c Claim) string {
	if c.Agent == "" {
		return "this session's main agent"
	}
	return "sibling leaf " + c.Agent
}

// cleanMessage refuses a non-Conventional subject, drops attribution trailers and trailing blanks, reporting each.
func cleanMessage(msg string, out io.Writer) (string, error) {
	lines := strings.Split(strings.ReplaceAll(msg, "\r\n", "\n"), "\n")
	subject := lines[0]
	if !shell.ConventionalSubject(subject) {
		return "", refuse("subject %q is not a Conventional Commit: expected <type>[(scope)][!]: <description> with "+
			"type feat|fix|docs|style|refactor|perf|test|chore|ci|build|revert", subject)
	}
	body, trimmed := trimBody(lines[1:])
	body, dropped := dropTrailers(body)
	body, _ = trimBody(body)
	if dropped > 0 {
		fmt.Fprintf(out, "message: dropped %d AI-attribution footer line(s)\n", dropped)
	}
	if trimmed {
		fmt.Fprintln(out, "message: trimmed trailing whitespace")
	}
	if n := shell.SubjectLen(subject); n > shell.SubjectWarnLen {
		fmt.Fprintf(out, "warning: subject is %d characters (over %d)\n", n, shell.SubjectWarnLen)
	}
	if n := proseLines(body); n > 2 {
		fmt.Fprintf(out, "warning: message has %d lines besides BREAKING CHANGE:/Fixes # lines (the rule is 2)\n", n)
	}
	return strings.Join(append([]string{subject}, body...), "\n"), nil
}

// trimBody strips trailing blanks from each line and from the end; it reports whether anything changed.
func trimBody(body []string) ([]string, bool) {
	body, changed := slices.Clone(body), false
	for i, l := range body {
		if t := strings.TrimRight(l, " \t"); t != l {
			body[i], changed = t, true
		}
	}
	for len(body) > 0 && body[len(body)-1] == "" {
		body, changed = body[:len(body)-1], true
	}
	return body, changed
}

// dropTrailers removes attribution lines from the message's last paragraph only; body text elsewhere stays.
func dropTrailers(body []string) ([]string, int) {
	start := 0
	for i, l := range slices.Backward(body) {
		if l == "" {
			start = i + 1
			break
		}
	}
	kept := slices.DeleteFunc(slices.Clone(body[start:]), shell.AttributionTrailer)
	return append(slices.Clone(body[:start]), kept...), len(body) - start - len(kept)
}

// proseLines counts the non-blank message lines that are not BREAKING CHANGE: or Fixes # lines.
func proseLines(body []string) int {
	n := 1
	for _, l := range body {
		if l != "" && !strings.HasPrefix(l, "BREAKING CHANGE:") && !strings.HasPrefix(l, "Fixes #") {
			n++
		}
	}
	return n
}

// printSet lists the set with +/- counts and what stays out or staged; it returns the set's untracked paths.
func printSet(ctx context.Context, out io.Writer, t commitTree, set []string) []string {
	counts := numstat(ctx, t.top, set)
	var untracked []string
	staged, left := 0, 0
	var partial []string
	for _, e := range t.entries {
		switch {
		case slices.Contains(set, e.Path):
			if e.Code != "??" && e.Code[0] != ' ' && e.Code[1] != ' ' {
				partial = append(partial, e.Path)
			}
			if e.Code == "??" {
				untracked = append(untracked, e.Path)
				counts[e.Path] = fileLines(t.abs(e.Path))
			}
		case e.Code != "??" && e.Code[0] != ' ':
			staged++
		default:
			left++
		}
	}
	for _, p := range set {
		fmt.Fprintf(out, "  %s  %s\n", p, countText(counts[p]))
	}
	if len(partial) > 0 {
		fmt.Fprintf(out, "note: %s is partly staged; the commit takes its whole working-tree content\n", strings.Join(partial, ", "))
	}
	if left > 0 {
		fmt.Fprintf(out, "left out: %d dirty files not in this commit\n", left)
	}
	if staged > 0 {
		fmt.Fprintf(out, "left staged: %d\n", staged)
	}
	return untracked
}

// countText renders "+a −d", "binary" for a numstat dash, "+? −?" when git gave no count.
func countText(c [2]string) string {
	switch c[0] {
	case "-":
		return "binary"
	case "":
		return "+? −?"
	}
	return "+" + c[0] + " −" + c[1]
}

// numstat is each set path's added and deleted line counts against HEAD; an unreadable diff leaves them empty.
func numstat(ctx context.Context, top string, set []string) map[string][2]string {
	out := map[string][2]string{}
	args := append([]string{"diff", "HEAD", "--numstat", "-z", "--no-renames", "--"}, literal(set)...)
	raw, err := proc.RunGit(ctx, top, args...)
	if err != nil {
		return out
	}
	for rec := range strings.SplitSeq(raw, "\x00") {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) == 3 {
			out[strings.TrimLeft(f[2], "\n")] = [2]string{f[0], f[1]}
		}
	}
	return out
}

// fileLines is an untracked file's added-line count; unreadable or binary content counts as binary.
func fileLines(path string) [2]string {
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return [2]string{"-", "-"}
	}
	n := bytes.Count(b, []byte("\n"))
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return [2]string{strconv.Itoa(n), "0"}
}

func literal(set []string) []string {
	out := make([]string, len(set))
	for i, p := range set {
		out[i] = ":(literal)" + p
	}
	return out
}

// runCommit adds only the set's untracked files and makes one pathspec commit; a failed commit restores the index.
func runCommit(ctx context.Context, top, msg string, set, untracked []string) (string, error) {
	var idx string
	var before, after []byte
	var mode os.FileMode
	if len(untracked) > 0 {
		var err error
		if idx, before, mode, err = indexSnapshot(ctx, top); err != nil {
			return "", err
		}
		if _, err = proc.RunGit(ctx, top, append([]string{"add", "--"}, literal(untracked)...)...); err != nil {
			return "", err
		}
		after, _ = os.ReadFile(idx)
	}
	res, err := proc.RunGit(ctx, top, append([]string{"commit", "-m", msg, "--"}, literal(set)...)...)
	if err != nil && len(untracked) > 0 {
		err = errors.Join(err, undoAdd(ctx, top, idx, before, after, mode, untracked))
	}
	return res, err
}

// indexSnapshot is the index file's path, bytes (nil when absent) and permission bits; a held index.lock refuses.
func indexSnapshot(ctx context.Context, top string) (string, []byte, os.FileMode, error) {
	idx, err := proc.RunGit(ctx, top, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", nil, 0, err
	}
	if idx = strings.TrimSpace(idx); !filepath.IsAbs(idx) {
		idx = filepath.Join(top, idx)
	}
	if _, err := os.Lstat(idx + ".lock"); err == nil {
		return "", nil, 0, refuse("%s.lock exists: another git process is writing the index", idx)
	}
	before, err := os.ReadFile(idx)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, 0, err
	}
	var mode os.FileMode = 0o644
	if fi, err := os.Stat(idx); err == nil {
		mode = fi.Mode().Perm() // taken before our add: git add rewrites the index as 0644
	}
	return idx, before, mode, nil
}

// Bounds for the unstage fallback: git's own lock holders are done in milliseconds.
var (
	unstageWait = 2 * time.Second
	unstageStep = 50 * time.Millisecond
	renameIndex = os.Rename
)

// undoAdd restores the index under git's own index.lock; with the lock held or the index changed, it unstages ours.
func undoAdd(ctx context.Context, top, idx string, before, after []byte, mode os.FileMode, untracked []string) error {
	lock := idx + ".lock"
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return unstage(ctx, top, untracked, "held by another process")
	}
	held := true // this run created the lock; only then may it remove it
	release := func() {
		if held {
			held = false
			_ = f.Close()
			_ = os.Remove(lock)
		}
	}
	defer release()
	if cur, _ := os.ReadFile(idx); !bytes.Equal(cur, after) {
		release()
		return unstage(ctx, top, untracked, "index changed since our add")
	}
	if err := restoreIndex(f, lock, idx, before, mode); err != nil {
		release()
		return unstage(ctx, top, untracked, err.Error())
	}
	held = false // renamed over the index (or removed with it): nothing of ours is left to remove
	return nil
}

// restoreIndex writes before into the held lock file, gives it the pre-add mode and renames it over idx.
func restoreIndex(f *os.File, lock, idx string, before []byte, mode os.FileMode) error {
	if len(before) == 0 { // the index did not exist before our add
		_ = f.Close()
		if err := os.Remove(idx); err != nil {
			return err
		}
		return os.Remove(lock)
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(before); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameIndex(lock, idx)
}

// unstage drops only our added files, retrying while git cannot take the lock; it never claims a missed unstage.
func unstage(ctx context.Context, top string, untracked []string, why string) error {
	args := append([]string{"rm", "--cached", "-f", "-q", "--"}, literal(untracked)...)
	deadline := time.Now().Add(unstageWait)
	var err error
	for {
		if _, err = proc.RunGit(ctx, top, args...); err == nil {
			return fmt.Errorf("index not restored byte for byte (%s): unstaged only the files this run added with git rm --cached", why)
		}
		if !time.Now().Add(unstageStep).Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(unstageStep):
			continue
		}
		break
	}
	list := strings.Join(untracked, " ")
	return errors.Join(fmt.Errorf("index not restored byte for byte (%s); files this run added remain staged: %s; "+
		"unstage them with: git rm --cached -f -- %s", why, list, list), err)
}
