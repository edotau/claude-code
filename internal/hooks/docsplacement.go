// `docs-placement` — PreToolUse(Write|Edit): BLOCKS a markdown write targeting a repo ROOT, or a
// <repo>/docs page git REFUSES to keep (ignored -> gone on a fresh clone), naming the canonical
// docs/ path instead. Root anchors, tracked files, and this harness's own docs tree pass.
package hooks

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// docsPlacementDisableEnv disables placement checks, not the independent plan-template gate.
const docsPlacementDisableEnv = "HARNESS_DOCS_PLACEMENT"

// rootAnchors are the root-level markdown files tooling requires THERE: GitHub/OSS conventions plus the
// cross-tool instruction files a repo may keep (Codex/Gemini/Copilot read AGENTS.md/GEMINI.md from a project root).
var rootAnchors = map[string]bool{
	"readme.md": true, "claude.md": true, "agents.md": true, "gemini.md": true,
	"license.md": true, "licence.md": true, "notice.md": true, "changelog.md": true,
	"contributing.md": true, "code_of_conduct.md": true, "security.md": true,
}

// DocsPlacement gates markdown writes at a repo root. Fails open on anything unclear (no path, not a
// git repo, unreadable git state) — placement is a convention worth enforcing, never worth wedging on.
func DocsPlacement(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	if code := docsPlacement(in, stderr); code != ExitProceed {
		return code
	}
	return checkPlanTemplate(in, stderr)
}

func docsPlacement(in *Input, stderr io.Writer) int {
	tool := strings.ToLower(in.ToolName)
	if tool != "" && tool != "write" && tool != "edit" && tool != "create" && tool != "apply_patch" {
		return ExitProceed
	}
	if os.Getenv(docsPlacementDisableEnv) == "0" {
		return ExitProceed
	}
	ctx, cancel := hookContext("docs-placement")
	defer cancel()
	if tool == "apply_patch" {
		var patch string
		for _, key := range []string{"patch", "input", "command"} {
			text := in.nested("tool_input", key)
			if key == "command" {
				text = in.ToolCommand()
			}
			if strings.Contains(text, "*** Begin Patch") {
				patch = text
				break
			}
		}
		for line := range strings.SplitSeq(patch, "\n") {
			for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Move to: "} {
				if path, ok := strings.CutPrefix(line, prefix); ok {
					if code := docsPlacementPath(ctx, in, strings.TrimSpace(path), stderr); code != ExitProceed {
						return code
					}
				}
			}
		}
		return ExitProceed
	}
	return docsPlacementPath(ctx, in, in.nested("tool_input", "file_path"), stderr)
}

func docsPlacementPath(ctx context.Context, in *Input, path string, stderr io.Writer) int {
	if path == "" || !isMarkdownPath(path) {
		return ExitProceed
	}
	if !filepath.IsAbs(path) && in.CWD != "" {
		path = filepath.Join(in.CWD, path)
	}
	// Canonicalize, don't just absolutize: a write spelled through a symlink would never compare
	// equal to the realpath git reports, and would walk straight past the gate.
	abs := canonDeep(path)
	dir, base := filepath.Dir(abs), filepath.Base(abs)
	if rootAnchors[strings.ToLower(base)] {
		return ExitProceed
	}
	// The harness's own docs/ holds only images + tracked knowledge; anything else there is a retired home.
	config := canonDeep(paths.ConfigDir())
	if harnessDocs := canonDeep(filepath.Join(config, "docs")); underDir(harnessDocs, abs) {
		if underDir(filepath.Join(harnessDocs, "knowledge"), abs) || underDir(filepath.Join(harnessDocs, "images"), abs) ||
			isTracked(ctx, config, strings.TrimPrefix(abs, config+string(filepath.Separator))) || ctx.Err() != nil {
			return ExitProceed
		}
		return blockPlacement(stderr, base, harnessDocs+" (retired: handoffs and reports live in the memory store)", config)
	}
	// Two gated shapes: a repo ROOT, and <root>/docs — the rest of the tree is the project's own.
	root := repoRootOf(ctx, dir)
	if root == "" {
		return ExitProceed
	}
	root = canonDeep(root)
	rel, ok := repoRel(root, abs)
	if !ok {
		return ExitProceed
	}
	atRoot := dir == root
	inDocs := strings.HasPrefix(rel, "docs/")
	if !atRoot && !inDocs {
		return ExitProceed
	}
	if isTracked(ctx, root, rel) || ctx.Err() != nil {
		return ExitProceed
	}
	// A docs/ page the repo COULD own is merely not-yet-added — the project's business. Only a path
	// git refuses to keep is stranded harness output: it is invisible on every fresh clone.
	if inDocs && !isIgnored(ctx, root, rel) {
		return ExitProceed
	}

	where := "the repo root (" + dir + ")"
	if inDocs {
		where = dir + ", which this repo gitignores — it vanishes on a fresh clone"
	}
	return blockPlacement(stderr, base, where, root)
}

// blockPlacement prints the canonical homes for a markdown write that landed somewhere stranded.
func blockPlacement(stderr io.Writer, base, where, root string) int {
	slug := paths.RepoSlug(root)
	plans := filepath.Join(paths.ConfigDir(), "plans")
	store := memory.Dir(root)
	fmt.Fprintf(stderr, "docs-placement: BLOCKED — %s would land in %s.\n"+
		"Harness-authored markdown belongs under %s (plans) or the memory store (session artifacts):\n"+
		"  plan             -> %s/YYYY-MM-DD-<slug>.md\n"+
		"  report/review    -> %s/YYYY-MM-DD-%s\n"+
		"  handoff          -> %s/YYYY-MM-DD-<topic>.md\n"+
		"  promoted, public-safe knowledge -> %s (tracked)\n"+
		"Re-issue the write at one of those paths. A genuine repo doc the user asked for: create it with\n"+
		"`git add -N <file>` first — tracked files pass, even inside an ignored dir. Or disable the gate\n"+
		"by setting %s=0.\n"+
		"-> rules/harness/docs-placement.md\n",
		base, where, plans,
		filepath.Join(plans, slug),
		memory.ReportsDir(store), base,
		memory.HandoffsDir(store),
		filepath.Join(paths.ConfigDir(), "docs", "knowledge"),
		docsPlacementDisableEnv)
	return ExitBlock
}

func isMarkdownPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown"
}

// repoRootOf returns dir's git toplevel, or "" when dir is not in a work tree. It climbs to the
// nearest EXISTING ancestor first: a Write creates its parent dirs, so a gate firing BEFORE the write
// is handed a path whose directory does not exist yet, and `git -C <missing dir>` just errors.
func repoRootOf(ctx context.Context, dir string) string {
	for dir != "" {
		if isDir(dir) {
			out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// canonDeep resolves p's deepest existing ancestor and re-joins the missing tail, so a nested target
// under a dir that does not exist yet (docs/knowledge/x/) still compares equal to the realpath git reports.
func canonDeep(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	tail := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// isTracked reports whether rel (repo-RELATIVE, slash-separated) is already git-tracked at root — an
// existing repo doc, so editing it is the user's business, not a placement violation.
func isTracked(ctx context.Context, root, rel string) bool {
	// :(literal) disables pathspec magic — a glob-shaped name ("*.md") would otherwise match some
	// OTHER tracked file and wave the untracked write straight through the gate.
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--", ":(literal)"+rel).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// isIgnored reports whether root's ignore rules exclude rel. check-ignore takes PATHNAMES, not
// pathspecs (":(literal)" is a fatal error there), and any failure reads as not-ignored: fail-open.
func isIgnored(ctx context.Context, root, rel string) bool {
	err := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "-q", "--", rel).Run()
	return err == nil
}

// repoRel is abs as a repo-relative slash path; false when abs escapes root.
func repoRel(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// underDir reports whether path is dir itself or below it (both already canonical).
func underDir(dir, path string) bool {
	return dir != "" && (path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)))
}
