// `agents fix`: the `make fmt` fixers, run only on the tracked files named by --path and not held by a peer.

package inspect

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// Exit codes of Fix; 1 is a skipped file, a missing tool or a failed fixer.
const (
	FixExitSkipped = 1
	FixExitUsage   = 2
)

type FixOptions struct {
	Dir      string
	Paths    []string
	DryRun   bool
	Source   worktree.ClaimSource // who counts as a peer when a file is claimed
	Out, Err io.Writer
}

// fixRun is one run's state: the files to fix, and what the fixers did to them.
type fixRun struct {
	root     string
	files    []string
	dry      bool
	covered  map[string]bool
	changed  []string // `path: tool` rows
	problems int      // skipped files, missing tools and failed fixers
	out, err io.Writer
}

// Fix runs each kind's fixer on the --path files; exit 0 fixed or clean, 1 anything skipped, 2 usage.
func Fix(o FixOptions) int {
	ctx, cancel := context.WithTimeout(context.Background(), diffGateTimeout)
	defer cancel()
	if len(o.Paths) == 0 {
		fmt.Fprintln(o.Err, "agents fix: give --path for each file or directory to fix; there is no whole-tree form")
		return FixExitUsage
	}
	r := &fixRun{dry: o.DryRun, covered: map[string]bool{}, out: o.Out, err: o.Err}
	if err := r.resolve(ctx, o); err != nil {
		fmt.Fprintf(o.Err, "agents fix: %v\n", err)
		return FixExitUsage
	}
	if err := r.dropHeld(ctx, o.Source); err != nil {
		fmt.Fprintf(o.Err, "agents fix: %v\n", err)
		return FixExitSkipped
	}
	for _, st := range steps {
		r.runStep(ctx, st)
	}
	return r.report()
}

// resolve turns --path into the tracked, plain files under it; an unusable named file is skipped with a line.
func (r *fixRun) resolve(ctx context.Context, o FixOptions) error {
	root, err := repoRoot(o.Dir)
	if err != nil {
		return err
	}
	sel, err := cleanPaths(root, o.Paths)
	if err != nil {
		return err
	}
	r.root = paths.Real(root)
	var specs []string
	named := map[string]bool{}
	for _, p := range sel {
		if strings.ContainsAny(p, "*?[") {
			return fmt.Errorf("--path %q: a file or directory, not a glob", p)
		}
		if st, err := os.Lstat(filepath.Join(r.root, p)); err == nil && st.IsDir() {
			p = strings.TrimSuffix(p, "/") + "/"
		} else {
			named[p] = true
		}
		if !r.plain(p) {
			r.skip(p, "not a plain file in the repository (missing, symlink or behind one)")
			continue
		}
		specs = append(specs, ":(literal)"+p)
	}
	return r.listTracked(ctx, specs, named)
}

// listTracked keeps the tracked plain files under specs and skips a named file git does not track.
func (r *fixRun) listTracked(ctx context.Context, specs []string, named map[string]bool) error {
	if len(specs) == 0 {
		return nil
	}
	out, err := proc.RunGitRaw(ctx, r.root, append([]string{"ls-files", "-z", "--"}, specs...)...)
	if err != nil {
		return err
	}
	for f := range strings.SplitSeq(out, "\x00") {
		switch {
		case f == "":
		case r.plain(f):
			r.files = append(r.files, f)
		case named[f]:
			r.skip(f, "a symlink")
		}
	}
	for p := range named {
		if !slices.Contains(r.files, p) && r.plain(p) {
			r.skip(p, "not tracked by git")
		}
	}
	slices.Sort(r.files)
	return nil
}

func (r *fixRun) skip(p, why string) {
	fmt.Fprintf(r.err, "%s: skipped, %s\n", p, why)
	r.problems++
}

// plain reports a repo file or directory that exists, is no symlink and has no symlinked parent.
func (r *fixRun) plain(rel string) bool {
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	st, err := os.Lstat(abs)
	if err != nil || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	real, err := filepath.EvalSymlinks(filepath.Dir(abs))
	return err == nil && real == filepath.Dir(abs)
}

// dropHeld removes the dirty files another live session or leaf has claimed, as `worktree commit` refuses them.
func (r *fixRun) dropHeld(ctx context.Context, src worktree.ClaimSource) error {
	if len(r.files) == 0 {
		return nil
	}
	raw, err := proc.RunGit(ctx, r.root, append([]string{"--no-optional-locks", "status", "--porcelain", "-z",
		"--untracked-files=no", "--"}, r.files...)...)
	if err != nil {
		return err
	}
	dirty := worktree.DirtyMap(r.root, proc.StatusEntries(raw))
	src.Leaf = func(worktree.Claim) string { return "" }
	if proc.InDispatch(ctx) { // a CLI leaf is refused every claim but its own dispatch's
		src.Leaf = func(c worktree.Claim) string { return "another agent of this session" }
		src.Dispatch = proc.Value(ctx, proc.DispatchIDEnv)
	}
	claims, err := worktree.LiveClaims(src)
	if err != nil && len(claims) == 0 {
		return fmt.Errorf("could not read file claims: %w", err)
	}
	var kept []string
	for _, f := range r.files {
		abs := filepath.Join(r.root, filepath.FromSlash(f))
		hits, err := worktree.FreshHits(ctx, r.root, abs, dirty, claims[abs], 0)
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			kept = append(kept, f)
			continue
		}
		r.skip(f, fmt.Sprintf("held by %s (since %s)", hits[0].Who, time.Unix(hits[0].TS, 0).Format("15:04")))
	}
	r.files = kept
	return nil
}

// runStep runs one table step's fixer, or with --dry-run its listing command, over the files its globs cover.
func (r *fixRun) runStep(ctx context.Context, st step) {
	files := match(r.files, st.globs)
	if st.fix == nil || len(files) == 0 {
		return
	}
	for _, f := range files {
		r.covered[f] = true
	}
	bin := st.fix[0]
	if st.install != "" {
		var ok bool
		if bin, ok = findTool(r.root, bin); !ok {
			fmt.Fprintf(r.err, "%s: skipped, not installed (%s)\n", st.fix[0], st.install)
			r.problems++
			return
		}
	}
	if r.dry {
		r.dryStep(ctx, st, bin, files)
		return
	}
	before := hashFiles(r.root, files)
	if out, err := r.runAll(ctx, bin, st.fix, files); err != nil {
		fmt.Fprintf(r.err, "%s: failed: %s\n", st.fix[0], firstLine(scrubPaths(out, r.root), err))
		r.problems++
	}
	for f, h := range before {
		if hashFiles(r.root, []string{f})[f] != h {
			r.changed = append(r.changed, f+": "+st.fix[0])
		}
	}
}

// dryStep lists the files the fixer would rewrite, from the check side's command; a file is listed once.
func (r *fixRun) dryStep(ctx context.Context, st step, bin string, files []string) {
	cmd, parse := st.cmd, st.parse
	if st.dry != nil {
		cmd = st.dry
	}
	if st.dryParse != nil {
		parse = st.dryParse
	}
	out, err := r.runAll(ctx, bin, cmd, files)
	rows := parse(scrubPaths(out, r.root))
	listed := 0
	for _, f := range rows {
		if slices.Contains(files, f.Path) && !slices.ContainsFunc(r.changed, func(c string) bool { return strings.HasPrefix(c, f.Path+": ") }) {
			r.changed = append(r.changed, f.Path+": "+st.fix[0])
			listed++
		}
	}
	if listed == 0 && len(rows) == 0 && err != nil {
		fmt.Fprintf(r.err, "%s: failed: %s\n", st.fix[0], firstLine(out, err))
		r.problems++
	}
}

// runAll runs cmd on every chunk of files, joining the output; every chunk runs, the first error is returned.
func (r *fixRun) runAll(ctx context.Context, bin string, cmd, files []string) (string, error) {
	var all strings.Builder
	var first error
	for chunk := range slices.Chunk(files, fileChunk) {
		out, err := gateRun(ctx, r.root, bin, expandArgs(cmd[1:], chunk, nil)...)
		all.WriteString(out)
		first = errors.Join(first, err)
	}
	return all.String(), first
}

func firstLine(out string, err error) string {
	if l := strings.TrimSpace(out); l != "" {
		return strings.SplitN(l, "\n", 2)[0]
	}
	return err.Error()
}

// hashFiles maps each file to its content hash; an unreadable file maps to "".
func hashFiles(root string, files []string) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f))); err == nil {
			sum := sha256.Sum256(b)
			out[f] = string(sum[:])
		}
	}
	return out
}

// report prints one row per changed file, then the summary; exit 1 when anything was skipped or failed.
func (r *fixRun) report() int {
	slices.Sort(r.changed)
	for _, c := range r.changed {
		fmt.Fprintln(r.out, c)
	}
	verb := "changed"
	if r.dry {
		verb = "would change"
	}
	touched := map[string]bool{}
	for _, c := range r.changed {
		touched[c[:strings.Index(c, ": ")]] = true
	}
	fmt.Fprintf(r.out, "%d file(s) %s, %d already clean, %d without a fixer, %d problem(s)\n",
		len(touched), verb, len(r.covered)-len(touched), len(r.files)-len(r.covered), r.problems)
	if r.problems > 0 {
		return FixExitSkipped
	}
	return 0
}
