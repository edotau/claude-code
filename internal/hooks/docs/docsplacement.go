// `docs-placement` blocks a markdown write to a repo ROOT or an untracked <repo>/docs page, naming the docs/ path.
package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// rootAnchors are the stems of the markdown files tooling requires at the repo root (README.md, README.markdown).
var rootAnchors = map[string]bool{
	"readme": true, "claude": true, "claude.local": true, "agents": true, "agents.override": true, "gemini": true,
	"license": true, "licence": true, "notice": true, "changelog": true,
	"contributing": true, "code_of_conduct": true, "security": true,
}

var placementTools = map[string]bool{"write": true, "edit": true, "multiedit": true, "create": true, "apply_patch": true, "bash": true}

// Placement is docs-placement: a placement block wins, else the plan path rewrite and template gate decide.
func Placement(in *core.Input) core.Result {
	if res, ok := noteRedirect(in); ok {
		return res
	}
	if res, ok := storeNameRedirect(in); ok {
		return res
	}
	dest, err := planDest(in)
	var placed core.Result
	if dest == "" { // a dated plan dest is rewritten (or blocked) below, never placement-denied
		var b bytes.Buffer
		if placed = core.ExitResult(placeFiles(in, &b), b.String()); placed.Decision == core.Block {
			return placed
		}
	}
	if tool := strings.ToLower(in.CanonicalTool()); tool != "write" && tool != "edit" {
		return placed
	}
	gated := planGate(in, dest, err)
	gated.Stderr = core.JoinNonEmpty("\n", placed.Stderr, gated.Stderr)
	return gated
}

// noteRedirect rewrites a top-level native note in the memory store into feedback/ or notes/, never denying.
func noteRedirect(in *core.Input) (core.Result, bool) {
	switch strings.ToLower(in.ToolName) {
	case "write", "edit", "multiedit", "read":
	default:
		return core.Result{}, false
	}
	from := in.Nested("tool_input", "file_path")
	abs := core.AnchorPath(from, in.CWD)
	base := filepath.Base(abs)
	folder := memory.NoteFolder(base)
	if from == "" || folder == "" || !paths.SameFile(filepath.Dir(abs), paths.MemoryStoreDir()) {
		return core.Result{}, false
	}
	updated := in.CloneToolInput()
	if updated == nil {
		return core.Result{}, false
	}
	dest := filepath.Join(paths.MemoryStoreDir(), folder, base)
	updated["file_path"] = dest
	return core.Result{UpdatedInput: updated, SystemMessage: "memory-note-path: " + from + " → " + dest}, true
}

// storeNameRedirect renames a NEW file in the store's handoffs/, reports/ or worktrees/ to YYYY-MM-DD-<name>.
func storeNameRedirect(in *core.Input) (core.Result, bool) {
	from := in.Nested("tool_input", "file_path")
	if from == "" || strings.ToLower(in.ToolName) != "write" {
		return core.Result{}, false
	}
	abs := core.AnchorPath(from, in.CWD)
	dir, base := filepath.Dir(abs), filepath.Base(abs)
	name := memory.DatedName(base, time.Now())
	if name == base { // pure string check first: most writes are already dated or exempt
		return core.Result{}, false
	}
	if _, err := os.Stat(abs); err == nil {
		return core.Result{}, false
	}
	hit, realDir, store := false, paths.FoldCase(paths.Real(dir)), paths.MemoryStoreDir()
	for _, f := range []string{"handoffs", "reports", "worktrees"} {
		hit = hit || realDir == paths.FoldCase(paths.Real(filepath.Join(store, f)))
	}
	updated := in.CloneToolInput()
	if !hit || updated == nil {
		return core.Result{}, false
	}
	dest := freeName(filepath.Join(dir, name))
	updated["file_path"] = dest
	return core.Result{UpdatedInput: updated, SystemMessage: "memory-store-name: " + from + " → " + dest}, true
}

// freeName returns path, or the first free <stem>-2<ext>, -3, … when path exists.
func freeName(path string) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for n, p := 2, path; ; n, p = n+1, fmt.Sprintf("%s-%d%s", stem, n, ext) {
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
}

// placeFiles gates markdown writes at a repo root; fails open on anything unclear — never worth wedging on.
func placeFiles(in *core.Input, stderr io.Writer) int {
	tool := strings.ToLower(in.ToolName)
	if tool != "" && !placementTools[tool] {
		return core.ExitProceed
	}
	ctx, cancel := core.HookContext("docs-placement")
	defer cancel()
	if tool == "apply_patch" {
		return docsPlacementPatch(ctx, in, stderr)
	}
	if tool == "bash" {
		return docsPlacementBash(ctx, in, stderr)
	}
	return docsPlacementPath(ctx, in, in.Nested("tool_input", "file_path"), stderr)
}

// docsPlacementBash checks every markdown file a shell command writes (redirect, tee, sed -i, cp, mv).
func docsPlacementBash(ctx context.Context, in *core.Input, stderr io.Writer) int {
	for _, path := range core.WrittenPathsPrecise(in.ToolCommand(), in.CWD) {
		if code := docsPlacementPath(ctx, in, path, stderr); code != core.ExitProceed {
			return code
		}
	}
	return core.ExitProceed
}

// docsPlacementPatch checks every file an apply_patch payload adds, updates or moves to.
func docsPlacementPatch(ctx context.Context, in *core.Input, stderr io.Writer) int {
	for _, e := range in.EditedPaths() {
		if e.Kind == textutil.PatchDelete {
			continue
		}
		if code := docsPlacementPath(ctx, in, e.Path, stderr); code != core.ExitProceed {
			return code
		}
	}
	return core.ExitProceed
}

func docsPlacementPath(ctx context.Context, in *core.Input, path string, stderr io.Writer) int {
	if path == "" || !isMarkdownPath(path) {
		return core.ExitProceed
	}
	path = core.AnchorPath(path, in.CWD)
	// Canonicalize: a write spelled through a symlink would never equal git's realpath and would pass the gate.
	abs := paths.Real(path)
	dir, base := filepath.Dir(abs), filepath.Base(abs)
	// The harness's own docs/ holds only images and templates; anything else there is a retired home.
	config := paths.Real(paths.ConfigDir())
	if harnessDocs := filepath.Join(config, "docs"); paths.Within(abs, harnessDocs) {
		return harnessDocsPlacement(ctx, stderr, config, harnessDocs, abs, base)
	}
	if paths.SameFile(dir, paths.MemoryStoreDir()) { // the store is its own git repo; notes at its root are the point
		return core.ExitProceed
	}
	return repoDocsPlacement(ctx, stderr, dir, base, abs)
}

// repoDocsPlacement gates the two shapes: a repo ROOT, and <root>/docs — the rest of the tree is the project's own.
func repoDocsPlacement(ctx context.Context, stderr io.Writer, dir, base, abs string) int {
	root, _ := paths.CheckoutOf(dir)
	if root == "" {
		return core.ExitProceed
	}
	rel, ok := core.RelWithin(root, abs)
	if !ok {
		return core.ExitProceed
	}
	rel = filepath.ToSlash(rel)
	atRoot := dir == root
	// anchors are exempt only at the repo root
	if atRoot && rootAnchors[strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))] {
		return core.ExitProceed
	}
	inDocs := strings.HasPrefix(paths.FoldCase(rel), "docs/")
	if !atRoot && !inDocs {
		return core.ExitProceed
	}
	if !stranded(ctx, root, rel, abs, inDocs) {
		return core.ExitProceed
	}

	where := "the repo root (" + dir + ")"
	if inDocs {
		where = dir + ", which this repo gitignores — it vanishes on a fresh clone"
	}
	return blockPlacement(stderr, base, where, root)
}

// harnessDocsPlacement gates a write under the harness's own docs/: images and templates and tracked files pass.
func harnessDocsPlacement(ctx context.Context, stderr io.Writer, config, harnessDocs, abs, base string) int {
	for _, tier := range []string{"images", "templates"} {
		if paths.Within(abs, filepath.Join(harnessDocs, tier)) {
			return core.ExitProceed
		}
	}
	if tracked, err := isTracked(ctx, config, strings.TrimPrefix(abs, config+string(filepath.Separator))); tracked || err != nil || ctx.Err() != nil {
		return core.ExitProceed
	}
	return blockPlacement(stderr, base, harnessDocs+" (retired: handoffs and reports live in the memory store)", config)
}

// stranded: a new untracked root file or ignored docs/ page; any existing file is the owner's.
func stranded(ctx context.Context, root, rel, abs string, inDocs bool) bool {
	_, statErr := os.Stat(abs)
	exists := statErr == nil
	if inDocs {
		ignored, _ := isIgnored(ctx, root, rel) // true only when git answered cleanly
		return !exists && ignored && ctx.Err() == nil
	}
	if exists {
		return false
	}
	// A git error (e.g. a `.git` git itself rejects) fails open: only clean git answers block a new file.
	ignored := make(chan error, 1)
	go func() { _, err := isIgnored(ctx, root, rel); ignored <- err }()
	tracked, err := isTracked(ctx, root, rel)
	ignoreErr := <-ignored
	return !tracked && err == nil && ignoreErr == nil && ctx.Err() == nil
}

// blockPlacement prints where a stranded markdown write belongs: the one exact path when its name picks a kind.
func blockPlacement(stderr io.Writer, base, where, root string) int {
	var fix string
	if dest := exactDest(base, root); dest != "" {
		fix = "Re-issue the write to " + dest + "."
	} else {
		fix = fmt.Sprintf("Plans belong in %s, reports in %s, handoffs in %s; re-issue the write there.",
			plans.Home(planRepoRoot(root)), paths.MemorySub("reports", "YYYY-MM-DD-<name>.md"), paths.MemorySub("handoffs", "YYYY-MM-DD-<topic>.md"))
	}
	fmt.Fprintf(stderr, "docs-placement: BLOCKED — %s would land in %s.\n"+
		"%s A genuine repo doc the user\n"+
		"asked for: `touch <file> && git add -N <file>` (`-f` if ignored), then re-issue the write.\n"+
		"Or launch with HARNESS_HOOK_SKIP=docs-placement.\n"+
		"-> rules/harness/docs-placement.md\n",
		base, where, fix)
	return core.ExitBlock
}

// exactDest is the free dated plan, report or handoff path a kind word in base picks ("" when it picks none).
func exactDest(base, root string) string {
	stem, day := strings.TrimSuffix(base, filepath.Ext(base)), time.Now().Format("2006-01-02")
	var prefix string
	switch {
	case hasNameToken(base, "plan", "plans"):
		prefix = filepath.Join(plans.Home(planRepoRoot(root)), day+"-"+paths.Slug(stem, "-", 60, "plan"))
	case hasNameToken(base, "report", "reports", "audit", "review", "findings"):
		prefix = paths.MemorySub("reports", day+"-"+paths.Slug(stem, "-", 60, "report"))
	case hasNameToken(base, "handoff", "handoffs"):
		prefix = paths.MemorySub("handoffs", day+"-"+paths.Slug(stem, "-", 60, "handoff"))
	default:
		return ""
	}
	dest, err := freeDated(prefix)
	if err != nil {
		return ""
	}
	return dest
}

func isMarkdownPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown"
}

// isTracked reports whether git lists rel (repo-RELATIVE, slash-separated) at root; err is git's failure.
func isTracked(ctx context.Context, root, rel string) (bool, error) {
	// :(literal) stops a glob-shaped name ("*.md") from matching another tracked file and passing the untracked write.
	out, err := proc.RunGit(ctx, root, "ls-files", "--", ":(literal)"+rel)
	return err == nil && strings.TrimSpace(out) != "", err
}

// isIgnored reports whether rel is ignored AND untracked (check-ignore skips tracked paths); err is git's failure.
func isIgnored(ctx context.Context, root, rel string) (bool, error) {
	_, err := proc.RunGit(ctx, root, "check-ignore", "-q", "--", rel)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}
