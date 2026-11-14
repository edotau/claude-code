package safety

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/shell"
)

// extraProtected are Edit-protected files the settings template does not list.
var extraProtected = []string{"~/.claude/settings.local.json"}

// protectedGlobs are the template's `Edit(...)` deny globs plus extraProtected: what Edit/Write may never touch.
var protectedGlobs = sync.OnceValue(func() []string {
	var doc struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	globs := append([]string(nil), extraProtected...)
	if json.Unmarshal(settings.Template(), &doc) != nil {
		return globs // the embedded template is tested; the extras still guard
	}
	for _, rule := range doc.Permissions.Deny {
		if g, ok := strings.CutPrefix(rule, "Edit("); ok && strings.HasSuffix(g, ")") {
			globs = append(globs, strings.TrimSuffix(g, ")"))
		}
	}
	return globs
})

// protectedWrite is the first path a Bash command writes or removes that Edit's deny rules protect ("" when none);
// a removal confined to temp folders is allowed (tempremoval.go), a write never is.
func protectedWrite(cmd, cwd string) string {
	for _, p := range core.WrittenPaths(cmd, cwd) {
		if protectedPath(p) {
			return p
		}
	}
	for _, p := range core.RemovedPaths(cmd, cwd) {
		if protectedPath(p) && !removalAllowed(cmd, cwd) {
			return p
		}
	}
	return ""
}

// protectedPath reports whether p, or its symlink-resolved form, falls under an Edit deny glob.
func protectedPath(p string) bool {
	return slices.ContainsFunc([]string{p, core.CanonPath(p)}, matchesProtected)
}

// interpreterWrites are the per-language write indicators; reads (open(p), readFile) match none.
var interpreterWrites = map[string]*regexp.Regexp{
	"python": regexp.MustCompile(`open\s*\([^\n]*?(,\s*|mode\s*=\s*)['"][rbt]*[wax+][rwxabt+]*['"]|` +
		`\b(write_text|write_bytes|unlink|copy\w*|move|replace|rename|remove|rmtree)\s*\(`),
	"js": regexp.MustCompile(`\b(writeFile|appendFile|createWriteStream|rename|unlink|rm|copyFile|truncate|writeTextFile|remove)` +
		`(Sync)?\s*\(|Bun\.write\b`),
	"script": regexp.MustCompile(`open\b[^\n]*(['"]\+?>>?|['"]\+?[wa]\+?['"])|` +
		`\b(File|IO)\.(binwrite|write|delete|unlink|rename)\b|\b(unlink|rename)\s*[(\s]|FileUtils\.`),
}

// interpreterLang maps an interpreter's basename to its interpreterWrites key ("" when not an interpreter).
func interpreterLang(base string) string {
	switch {
	case strings.HasPrefix(base, "python"):
		return "python"
	case base == "node" || base == "deno" || base == "bun":
		return "js"
	case base == "perl" || base == "ruby":
		return "script"
	}
	return ""
}

// interpreterWrite is the protected file an interpreter command's text mentions beside a write indicator, else "".
func interpreterWrite(cmd string) string {
	cmd = core.DropDataHeredocs(cmd)
	for _, seg := range shell.Segments(cmd) {
		rest, _ := shell.Unwrap(shell.Tokens(seg), "")
		if len(rest) == 0 {
			continue
		}
		re := interpreterWrites[interpreterLang(filepath.Base(rest[0]))]
		if re == nil || !re.MatchString(cmd) {
			continue
		}
		for _, g := range protectedGlobs() {
			needle := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(g, "**/"), "**"), "~/")
			if needle != "" && strings.Contains(cmd, needle) {
				return needle
			}
		}
	}
	return ""
}

// matchesProtected reports whether abs falls under a deny glob: `**/x` by suffix, `dir/**` by prefix, else exact.
func matchesProtected(abs string) bool {
	for _, g := range protectedGlobs() {
		g = paths.ExpandHome(g)
		switch {
		case strings.HasPrefix(g, "**/"):
			if strings.HasSuffix(abs, string(filepath.Separator)+g[3:]) {
				return true
			}
		case strings.HasSuffix(g, "/**"):
			if paths.Within(abs, strings.TrimSuffix(g, "/**")) {
				return true
			}
		case abs == g:
			return true
		}
	}
	return false
}
