// Protected edits — safety-scan's settings.json Edit/Write deny globs on an ask-less agent's edit tools.

package safety

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/permission"
)

// editTools are the canonical hook tool names (codex's apply_patch is Edit) that write a file.
var editTools = map[string]bool{"write": true, "edit": true, "multiedit": true, "write_file": true, "edit_file": true}

// protectedEdit enforces the settings deny globs on file edits, including apply_patch; other tools get protectedTool.
func protectedEdit(in *core.Input) core.Result {
	if !editTools[strings.ToLower(in.CanonicalTool())] {
		return protectedTool(in)
	}
	p := livePolicy()
	allowed := func(f string) bool { return tempWriteAllowed("", f) }
	if f, _ := pathHit(editPaths(in), foldedGlobs(p.editDeny, in.CWD, p.twin), allowed); f != "" {
		return core.Result{Decision: core.Block, Floor: true, Reason: core.SafetyMessage(in, core.Block,
			assets.SafetyProtectedWhat(), f, assets.SafetyProtectedHint())}
	}
	return core.Result{}
}

// pathTools are the non-edit tools whose deny rows are path globs, anchored like Edit/Write globs.
var pathTools = map[string]bool{"Read": true, "Grep": true, "Glob": true}

// protectedTool enforces the settings deny rows of a non-edit tool (Read, Grep, Glob, WebFetch…) on its own arguments.
func protectedTool(in *core.Input) core.Result {
	tool := permission.CanonicalTool(in.CanonicalTool())
	p := livePolicy()
	if pathTools[tool] {
		return protectedRead(in, tool, p)
	}
	var rules []permission.Rule
	for _, r := range p.toolDeny {
		if r.Tool == tool {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return core.Result{}
	}
	arg, problem := toolArg(in, tool)
	if problem != "" {
		return toolBlock(in, problem, tool)
	}
	if (permission.Permissions{Mode: permission.PermAllow, Deny: rules}).DecideAt(in.CWD, tool, arg) == permission.Deny {
		return toolBlock(in, assets.SafetyDenyWhat(), tool+" "+arg)
	}
	return core.Result{}
}

func toolBlock(in *core.Input, what, detail string) core.Result {
	return core.Result{Decision: core.Block, Floor: true, Reason: core.SafetyMessage(in, core.Block, what, detail,
		assets.SafetyDenyHint())}
}

// toolArg is the argument a non-path deny row matches: the URL, the query, else none; a misshapen one is a problem.
func toolArg(in *core.Input, tool string) (arg, problem string) {
	key := map[string]string{"WebFetch": "url", "WebSearch": "query"}[tool]
	if key == "" {
		return "", ""
	}
	v, ok := in.ToolInput()[key].(string)
	if !ok || v == "" {
		return "", assets.SafetyArgMissing(tool, key)
	}
	return v, ""
}

// protectedRead judges a Read, Grep or Glob call's own path against the tool's anchored deny globs.
func protectedRead(in *core.Input, tool string, p policy) core.Result {
	var raw []string
	for _, r := range p.pathDeny {
		if r.tool == tool {
			raw = append(raw, r.glob)
		}
	}
	if len(raw) == 0 {
		return core.Result{}
	}
	forms, problem := toolPaths(in, tool)
	if problem != "" {
		return toolBlock(in, problem, tool)
	}
	hit, resolved := pathHit(forms, foldedGlobs(raw, in.CWD, p.twin), func(string) bool { return false })
	if hit != "" {
		return toolBlock(in, assets.SafetyDenyWhat(), tool+" "+hit)
	}
	if !resolved {
		return toolBlock(in, assets.SafetyPathUnresolved(), tool)
	}
	return core.Result{}
}

// pathHit is the first absolute spelling in forms a glob covers and allowed refuses; resolved: one was absolute.
func pathHit(forms, globs []string, allowed func(string) bool) (hit string, resolved bool) {
	for _, form := range forms { // pathForms already hold the symlink-resolved spelling
		f := paths.ExpandHome(form)
		if !filepath.IsAbs(f) {
			continue
		}
		resolved = true
		if (f == form && matchesProtected(f, globs) || f != form && protectedPath(f, globs)) && !allowed(f) {
			return f, true
		}
	}
	return "", resolved
}

// toolPaths are the spellings of the one path a Read, Grep or Glob call names; Grep and Glob default to the cwd.
func toolPaths(in *core.Input, tool string) ([]string, string) {
	input := in.ToolInput()
	for _, key := range []string{"file_path", "filePath", "absolute_path", "path"} {
		v, present := input[key]
		if !present || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return nil, tool + " path is not a string; blocked"
		}
		if s != "" {
			return pathForms(in.CWD, s), ""
		}
	}
	if tool == "Read" || in.CWD == "" {
		return nil, tool + " call names no path; blocked"
	}
	return pathForms(in.CWD, in.CWD), ""
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
