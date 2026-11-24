// Protected edits — safety-scan's settings.json Edit/Write deny globs on an ask-less agent's edit tools.

package safety

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
)

// editTools are the canonical hook tool names (codex's apply_patch is Edit) that write a file.
var editTools = map[string]bool{"write": true, "edit": true, "multiedit": true, "write_file": true, "edit_file": true}

// protectedEdit enforces the settings deny globs on file edits, including apply_patch; other tools get protectedTool.
func protectedEdit(in *core.Input) core.Result {
	if !editTools[strings.ToLower(in.CanonicalTool())] {
		return protectedTool(in)
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

// protectedTool enforces the settings deny rows of a non-edit tool (Read, Grep, Glob, WebFetch…) on its path or URL.
func protectedTool(in *core.Input) core.Result {
	tool := permission.CanonicalTool(in.CanonicalTool())
	var rules []permission.Rule
	for _, r := range livePolicy().toolDeny {
		if r.Tool == tool {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return core.Result{}
	}
	judge := permission.Permissions{Mode: permission.PermAllow, Deny: rules}
	for _, arg := range toolArgs(in, tool) {
		if judge.DecideAt(in.CWD, tool, arg) == permission.Deny {
			return core.Result{Decision: core.Block, Floor: true, Reason: core.SafetyMessage(in, core.Block,
				"matches a settings.json deny rule", tool+" "+arg, "Change permissions.deny in ~/.claude/settings.json if this is wrong.")}
		}
	}
	return core.Result{}
}

// toolArgs are the spellings a deny row's argument is matched against: the URL, the query, or every path form.
func toolArgs(in *core.Input, tool string) []string {
	switch tool {
	case "WebFetch":
		return []string{in.Nested("tool_input", "url")}
	case "WebSearch":
		return []string{in.Nested("tool_input", "query")}
	}
	var out []string
	for _, f := range editPaths(in) {
		if f == "" {
			f = in.CWD // grep, glob and list search the cwd when no path is given
		}
		out = append(out, f)
	}
	return out
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
