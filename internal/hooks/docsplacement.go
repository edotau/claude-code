// `docs-placement` — PreToolUse(Write|Edit): BLOCKS a markdown write targeting a repo ROOT, or a
// <repo>/docs page git REFUSES to keep (ignored -> gone on a fresh clone), naming the canonical
// docs/ path instead. Root anchors, tracked files, and this harness's own docs tree pass.
package hooks

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// docsPlacementDisableEnv is the sole knob: set to "0" to let every markdown write through unchecked.
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
	if in.ToolName != "" && in.ToolName != "Write" && in.ToolName != "Edit" {
		return ExitProceed
	}
	if os.Getenv(docsPlacementDisableEnv) == "0" {
		return ExitProceed
	}
	path := in.nested("tool_input", "file_path")
	if path == "" || !isMarkdownPath(path) {
		return ExitProceed
	}
	// Canonicalize, don't just absolutize: a write spelled through a symlink would never compare
	// equal to the realpath git reports, and would walk straight past the gate.
	abs := canonDeep(path)
	dir, base := filepath.Dir(abs), filepath.Base(abs)
	if rootAnchors[strings.ToLower(base)] {
		return ExitProceed
	}
	// This harness's own docs tree passes unconditionally: docs/{handoffs,reports,memory} are
	// gitignored INSIDE this checkout, so the gated shapes below would block every legit write.
	if underDir(canonDeep(filepath.Join(paths.ConfigDir(), "docs")), abs) {
		return ExitProceed
	}
	// Two gated shapes: a repo ROOT, and <root>/docs — the rest of the tree is the project's own.
	root := repoRootOf(dir)
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
	if isTracked(root, rel) {
		return ExitProceed
	}
	// A docs/ page the repo COULD own is merely not-yet-added — the project's business. Only a path
	// git refuses to keep is stranded harness output: it is invisible on every fresh clone.
	if inDocs && !isIgnored(root, rel) {
		return ExitProceed
	}

	where := fmt.Sprintf("the repo root (%s)", dir)
	if inDocs {
		where = fmt.Sprintf("%s, which this repo gitignores — it vanishes on a fresh clone", dir)
	}
	slug := paths.RepoSlug(root)
	docs := filepath.Join(paths.ConfigDir(), "docs")
	plans := filepath.Join(paths.ConfigDir(), "plans")
	fmt.Fprintf(stderr, "docs-placement: BLOCKED — %s would land in %s.\n"+
		"Harness-authored markdown belongs under %s (plans) or %s (everything else):\n"+
		"  plan             -> %s/<date>-<title>.md\n"+
		"  report/review    -> %s/%s\n"+
		"  handoff beacon   -> %s (machine-written at session start — don't hand-author one)\n"+
		"Re-issue the write at one of those paths. A genuine repo doc the user asked for: create it with\n"+
		"`git add -N <file>` first — tracked files pass, even inside an ignored dir. Or disable the gate\n"+
		"by setting %s=0.\n"+
		"-> rules/standards/documentation.md\n",
		base, where, plans, docs,
		filepath.Join(plans, slug),
		filepath.Join(docs, "reports"), base,
		filepath.Join(docs, "handoffs"),
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
func repoRootOf(dir string) string {
	for dir != "" {
		if isDir(dir) {
			return memory.GitToplevel(dir)
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
// under a dir that does not exist yet (docs/reports/) still compares equal to the realpath git reports.
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
func isTracked(root, rel string) bool {
	// :(literal) disables pathspec magic — a glob-shaped name ("*.md") would otherwise match some
	// OTHER tracked file and wave the untracked write straight through the gate.
	out, err := exec.Command("git", "-C", root, "ls-files", "--", ":(literal)"+rel).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// isIgnored reports whether root's ignore rules exclude rel. check-ignore takes PATHNAMES, not
// pathspecs (":(literal)" is a fatal error there), and any failure reads as not-ignored: fail-open.
func isIgnored(root, rel string) bool {
	err := exec.Command("git", "-C", root, "check-ignore", "-q", "--", rel).Run()
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
