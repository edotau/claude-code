package safety

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
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

// protectedWrite is the first path a Bash command writes that Edit's deny rules protect ("" when none).
func protectedWrite(cmd, cwd string) string {
	if !core.MayWrite(cmd) {
		return ""
	}
	for _, p := range core.WrittenPaths(cmd, cwd) {
		for _, abs := range []string{p, core.CanonPath(p)} {
			if matchesProtected(abs) {
				return p
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
