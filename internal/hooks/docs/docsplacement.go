// `docs-placement` — PreToolUse(Write|Edit): BLOCKS a markdown write targeting a repo ROOT, or a
// <repo>/docs page git REFUSES to keep (ignored -> gone on a fresh clone), naming the canonical
// docs/ path instead. Root anchors, tracked files, and this harness's docs/images + docs/templates pass.
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

// docsPlacementDisableEnv disables placement checks.
const docsPlacementDisableEnv = "HARNESS_DOCS_PLACEMENT"

// rootAnchors are the root-level markdown files tooling requires THERE: GitHub/OSS conventions plus the
// cross-tool instruction files a repo may keep (Codex/Gemini/Copilot read AGENTS.md/GEMINI.md from a project root).
var rootAnchors = map[string]bool{
	"readme.md": true, "claude.md": true, "agents.md": true, "gemini.md": true,
	"license.md": true, "licence.md": true, "notice.md": true, "changelog.md": true,
	"contributing.md": true, "code_of_conduct.md": true, "security.md": true,
}

// Placement gates markdown writes at a repo root. Fails open on anything unclear (no path, not a
// git repo, unreadable git state) — placement is a convention worth enforcing, never worth wedging on.
func Placement(in *core.Input, stderr io.Writer) int {
	tool := strings.ToLower(in.ToolName)
	if tool != "" && tool != "write" && tool != "edit" && tool != "create" && tool != "apply_patch" {
		return core.ExitProceed
	}
	if os.Getenv(docsPlacementDisableEnv) == "0" {
		return core.ExitProceed
	}
	ctx, cancel := core.HookContext("docs-placement")
	defer cancel()
	if tool == "apply_patch" {
		return docsPlacementPatch(ctx, in, stderr)
	}
	return docsPlacementPath(ctx, in, in.Nested("tool_input", "file_path"), stderr)
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
	// Canonicalize, don't just absolutize: a write spelled through a symlink would never compare
	// equal to the realpath git reports, and would walk straight past the gate.
	abs := core.CanonPath(path)
	dir, base := filepath.Dir(abs), filepath.Base(abs)
	if rootAnchors[strings.ToLower(base)] {
		return core.ExitProceed
	}
	// The harness's own docs/ holds only images and templates; anything else there is a retired home.
	config := core.CanonPath(paths.ConfigDir())
	if harnessDocs := core.CanonPath(filepath.Join(config, "docs")); paths.Within(abs, harnessDocs) {
		return harnessDocsPlacement(ctx, stderr, config, harnessDocs, abs, base)
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
	inDocs := strings.HasPrefix(rel, "docs/")
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

// stranded: an untracked root file or ignored docs/ page; an existing ignored file is the owner's local doc.
func stranded(ctx context.Context, root, rel, abs string, inDocs bool) bool {
	_, statErr := os.Stat(abs)
	exists := statErr == nil
	if inDocs {
		ignored, _ := isIgnored(ctx, root, rel) // true only when git answered cleanly
		return !exists && ignored && ctx.Err() == nil
	}
	// A git error (e.g. a `.git` git itself rejects) fails open: only a clean empty listing blocks.
	if tracked, err := isTracked(ctx, root, rel); tracked || err != nil || ctx.Err() != nil {
		return false
	}
	ignored, err := isIgnored(ctx, root, rel)
	if err != nil || ctx.Err() != nil {
		return false
	}
	return !exists || !ignored
}

// blockPlacement prints the canonical homes for a markdown write that landed somewhere stranded.
func blockPlacement(stderr io.Writer, base, where, root string) int {
	plansDir := filepath.Join(paths.ConfigDir(), "plans")
	store := memory.Dir(root)
	if paths.Within(core.CanonPath(root), core.CanonPath(memory.StoreDir())) {
		store = root // a repo inside the memory store is its own bank, not the home one
	}
	plansHome := plans.Home(planRepoRoot(root))
	fmt.Fprintf(stderr, "docs-placement: BLOCKED — %s would land in %s.\n"+
		"Harness-authored markdown belongs under %s (plans) or the memory store (session artifacts):\n"+
		"  plan             -> %s/YYYY-MM-DD-<slug>.md\n"+
		"  report/review    -> %s/YYYY-MM-DD-%s\n"+
		"  handoff          -> %s/YYYY-MM-DD-<topic>.md\n"+
		"  promoted knowledge -> %s/knowledge/<topic>.md (local-only)\n"+
		"Re-issue the write at one of those paths. A genuine repo doc the user asked for: create it with\n"+
		"`git add -N <file>` first — tracked files pass, even inside an ignored dir. Or disable the gate\n"+
		"by setting %s=0.\n"+
		"-> rules/harness/docs-placement.md\n",
		base, where, plansDir,
		plansHome,
		memory.ReportsDir(store), base,
		memory.HandoffsDir(store),
		store,
		docsPlacementDisableEnv)
	return core.ExitBlock
}

func isMarkdownPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown"
}

// isTracked reports whether git lists rel (repo-RELATIVE, slash-separated) at root; err is git's failure.
func isTracked(ctx context.Context, root, rel string) (bool, error) {
	// :(literal) disables pathspec magic — a glob-shaped name ("*.md") would otherwise match some
	// OTHER tracked file and wave the untracked write straight through the gate.
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
