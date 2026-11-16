// `docs-placement` blocks a markdown write to a repo ROOT or an untracked <repo>/docs page, naming the docs/ path.
package docs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
)

// rootAnchors are the stems of the markdown files tooling requires at the repo root (README.md, README.markdown).
var rootAnchors = map[string]bool{
	"readme": true, "claude": true, "claude.local": true, "agents": true, "agents.override": true, "gemini": true,
	"license": true, "licence": true, "notice": true, "changelog": true,
	"contributing": true, "code_of_conduct": true, "security": true,
}

// Placement gates markdown writes at a repo root; fails open on anything unclear — never worth wedging on.
func Placement(in *core.Input, stderr io.Writer) int {
	tool := strings.ToLower(in.ToolName)
	if tool != "" && tool != "write" && tool != "edit" && tool != "multiedit" && tool != "create" && tool != "apply_patch" && tool != "bash" {
		return core.ExitProceed
	}
	if strandedPlanDest(in) != "" { // docs-plan-gate rewrites (or blocks) this write; a parallel deny would mask it
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
	for _, path := range core.WrittenPaths(in.ToolCommand(), in.CWD) {
		if code := docsPlacementPath(ctx, in, path, stderr); code != core.ExitProceed {
			return code
		}
	}
	return core.ExitProceed
}

// applyPatchText returns the first apply_patch payload carrying a patch header ("" when there is none).
func applyPatchText(in *core.Input) string {
	for _, key := range []string{"patch", "input", "command"} {
		text := in.Nested("tool_input", key)
		if key == "command" {
			text = in.ToolCommand()
		}
		if strings.Contains(text, "*** Begin Patch") {
			return text
		}
	}
	return ""
}

// docsPlacementPatch checks every file an apply_patch payload adds, updates or moves to.
func docsPlacementPatch(ctx context.Context, in *core.Input, stderr io.Writer) int {
	for line := range strings.SplitSeq(applyPatchText(in), "\n") {
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Move to: "} {
			path, ok := strings.CutPrefix(line, prefix)
			if !ok {
				continue
			}
			if code := docsPlacementPath(ctx, in, strings.TrimSpace(path), stderr); code != core.ExitProceed {
				return code
			}
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
	if harnessDocs := paths.Real(filepath.Join(config, "docs")); paths.Within(abs, harnessDocs) {
		return harnessDocsPlacement(ctx, stderr, config, harnessDocs, abs, base)
	}
	if paths.SameFile(dir, memory.StoreDir()) { // the store is its own git repo; notes at its root are the point
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
	if atRoot && rootAnchors[strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))] { // anchors are exempt only at the repo root
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
	if tracked, err := isTracked(ctx, root, rel); tracked || err != nil || ctx.Err() != nil {
		return false
	}
	_, err := isIgnored(ctx, root, rel)
	return err == nil && ctx.Err() == nil
}

// blockPlacement prints the canonical homes for a markdown write that landed somewhere stranded.
func blockPlacement(stderr io.Writer, base, where, root string) int {
	b := memory.BankFor(root)
	reports, handoffs := b.Sub("reports", "YYYY-MM-DD-<name>.md"), b.Sub("handoffs", "YYYY-MM-DD-<topic>.md")
	plansHome := plans.Home(planRepoRoot(root))
	fmt.Fprintf(stderr, "docs-placement: BLOCKED — %s would land in %s.\n"+
		"Plans belong in %s, reports in %s, handoffs in %s; re-issue the write there. A genuine repo doc the user\n"+
		"asked for: `touch <file> && git add -N <file>` (`-f` if ignored), then re-issue the write.\n"+
		"Or launch with HARNESS_HOOK_SKIP=docs-placement.\n"+
		"-> rules/harness/docs-placement.md\n",
		base, where, plansHome, reports, handoffs)
	return core.ExitBlock
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
