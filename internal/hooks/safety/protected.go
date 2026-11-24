// Protected edits — safety-scan's settings.json Edit/Write deny globs on an ask-less agent's edit tools.

package safety

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
)

// editTools are the canonical hook tool names (codex's apply_patch is Edit) that write a file.
var editTools = map[string]bool{"write": true, "edit": true, "multiedit": true, "write_file": true, "edit_file": true}

// protectedEdit enforces the settings deny globs on file edits, including apply_patch.
func protectedEdit(in *core.Input) core.Result {
	if !editTools[strings.ToLower(in.CanonicalTool())] {
		return core.Result{}
	}
	globs := foldedGlobs(livePolicy().editDeny, in.CWD)
	for _, form := range editPaths(in) { // pathForms already hold the symlink-resolved spelling
		f := paths.ExpandHome(form)
		if !filepath.IsAbs(f) {
			continue
		}
		if (matchesProtected(f, globs) || f != form && protectedPath(f, globs)) && !tempWriteAllowed("", f) {
			return core.Result{Decision: core.Block, Floor: true, Reason: core.SafetyMessage(in, core.Block,
				"writes a file Edit/Write may never touch", f, "Edit it by hand, or run it yourself in a terminal.")}
		}
	}
	return core.Result{}
}

// editPaths are the spellings of every path the call edits, one set per file an apply_patch touches.
func editPaths(in *core.Input) []string {
	var out []string
	for _, e := range in.EditedPaths() {
		out = append(out, pathForms(in.CWD, e.Path)...)
	}
	if len(out) > 0 {
		return out
	}
	return pathForms(in.CWD, "")
}

// pathForms are p's cwd-relative and absolute spellings, as typed and symlink-resolved; "" stays "".
func pathForms(cwd, p string) []string {
	if p == "" {
		return []string{""}
	}
	abs := paths.AnchorPath(p, cwd)
	forms := []string{relToCWD(cwd, p), abs}
	if filepath.IsAbs(abs) {
		canonCWD := cwd
		if cwd != "" {
			canonCWD = paths.Real(cwd)
		}
		canon := paths.Real(abs)
		forms = append(forms, relToCWD(canonCWD, canon), canon)
	}
	var out []string
	for _, f := range forms {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// relToCWD is p relative to cwd when inside it, else p unchanged (copilot's relToWorkDir).
func relToCWD(cwd, p string) string {
	if rel, err := filepath.Rel(cwd, p); cwd != "" && filepath.IsAbs(p) && err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return p
}
