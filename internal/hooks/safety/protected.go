package safety

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

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
// a write or removal confined to temp folders is allowed (tempremoval.go).
func protectedWrite(cmd, cwd string) string {
	for _, p := range core.WrittenPaths(cmd, cwd) {
		if protectedPath(p) && !tempWriteAllowed(cmd, p) {
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
	return slices.ContainsFunc([]string{p, paths.Real(p)}, matchesProtected)
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
		lang := interpreterLang(filepath.Base(rest[0]))
		if re := interpreterWrites[lang]; re == nil || !scriptWrites(lang, re, cmd) {
			continue
		}
		for _, g := range protectedGlobs() {
			needle := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(g, "**/"), "**"), "~/")
			if needle != "" && strings.Contains(paths.FoldCase(cmd), paths.FoldCase(needle)) {
				return needle
			}
		}
	}
	return ""
}

// pyIndirectModule spots os/shutil reached by another name, which would make a module call read as a method call.
var pyIndirectModule = regexp.MustCompile(`\bimport\s+(os|shutil|posix|nt)\s+as\b|` +
	`\bfrom\s+(os|shutil|posix|nt)\s+import\b|__import__|import_module|getattr`)

// scriptWrites reports a write indicator for lang in cmd; a Python str/list .replace(a, b)/.remove(x) is none.
func scriptWrites(lang string, re *regexp.Regexp, cmd string) bool {
	if lang != "python" || pyIndirectModule.MatchString(cmd) {
		return re.MatchString(cmd)
	}
	for _, m := range re.FindAllStringSubmatchIndex(cmd, -1) {
		if m[4] < 0 || !pyDataCall(cmd, m[4], m[5], m[1]) { // m[4:6]: the write-call name group
			return true
		}
	}
	return false
}

// pyDataCall reports str.replace(a, b) or list.remove(x), not os.replace or one-argument Path.replace(target).
func pyDataCall(cmd string, start, end, args int) bool {
	name := cmd[start:end]
	if name != "replace" && name != "remove" || start == 0 || cmd[start-1] != '.' {
		return false
	}
	qual := cmd[:start-1]
	notIdent := func(r rune) bool { return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	qual = qual[strings.LastIndexFunc(qual, notIdent)+1:]
	if slices.Contains([]string{"os", "shutil", "posix", "nt"}, qual) {
		return false
	}
	n, ok := pyArgCount(cmd, args)
	if name == "replace" {
		return ok && n >= 2
	}
	return ok && n >= 1
}

// pyArgCount counts the top-level arguments of the call whose `(` ends at cmd[from-1]; ok=false when unclosed.
func pyArgCount(cmd string, from int) (n int, ok bool) {
	depth, quote, empty := 0, byte(0), true
	for i := from; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0 && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote, empty = c, false
		case strings.IndexByte("([{", c) >= 0:
			depth, empty = depth+1, false
		case strings.IndexByte(")]}", c) >= 0 && depth > 0:
			depth--
		case strings.IndexByte(")]}", c) >= 0 && empty:
			return 0, true
		case strings.IndexByte(")]}", c) >= 0:
			return n + 1, true
		case c == ',' && depth == 0:
			n++
		case c != ' ' && c != '\t':
			empty = false
		}
	}
	return 0, false
}

// matchesProtected reports whether abs falls under a deny glob: `**/x` by suffix, `dir/**` by prefix, else exact.
func matchesProtected(abs string) bool {
	abs = paths.FoldCase(abs)
	for _, g := range protectedGlobs() {
		g = paths.FoldCase(paths.ExpandHome(g))
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
