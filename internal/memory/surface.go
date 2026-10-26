package memory

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	mdast "github.com/jeduden/mdsmith/pkg/goldmark/ast"
	"github.com/jeduden/mdsmith/pkg/goldmark/parser"
	"github.com/jeduden/mdsmith/pkg/markdown"
)

// Surface caps: a character budget under hooks' 9,500-char additionalContext cap
// (~1,000 left for the note). The index's slice is reserved so the current pair can never starve it.
const (
	TotalCapChars = 8500
	indexCapChars = 2500
)

const truncatedNotice = "\n<!-- (surface truncated) -->"

// SessionSurface composes root's local bank surface plus the home one, capped at TotalCapChars; "" when empty.
func SessionSurface(root string) string {
	return unifiedSurface(root)
}

// CapChars cuts s to at most max runes (marker included) at the last whole line.
func CapChars(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	room := max - len([]rune(truncatedNotice))
	if room <= 0 {
		return ""
	}
	cut := string(r[:room])
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + truncatedNotice
}

// Unified-store budgets (plan §3.4): the catalog and the focus slices are reserved so neither starves the other.
const (
	catalogCapChars = 2500
	focusCapChars   = 2000
)

// unifiedSurface is the session preload: the catalog router plus root's focus sections; Claude Code loads
// the repo's MEMORY.md (with its generated bank region) itself, so no index is injected here.
func unifiedSurface(root string) string {
	dir := Dir(root)
	var parts []string
	if data, err := os.ReadFile(filepath.Join(StoreDir(), CatalogFile)); err == nil {
		parts = append(parts, "<!-- MEMORY CATALOG (start here; "+StoreDir()+") -->\n"+capRows(strings.TrimSpace(stripCatalogOrigins(string(data))), catalogCapChars))
	}
	var focus []string
	if data, err := os.ReadFile(filepath.Join(dir, "activeContext.md")); err == nil {
		for _, h := range []string{"Current Focus", "Blockers", "Next Steps"} {
			if sec := sectionBody(string(data), h); sec != "" {
				focus = append(focus, "## "+h+"\n"+sec)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "progress.md")); err == nil {
		if sec := sectionBody(string(data), "In Progress"); sec != "" {
			focus = append(focus, "## In Progress\n"+sec)
		}
	}
	if len(focus) > 0 {
		parts = append(parts, CapChars("<!-- THIS REPO ("+dir+") — focus only; read the typed files on demand -->\n"+strings.Join(focus, "\n\n"), focusCapChars))
	}
	if !nativeLinked(root, dir) { // Claude Code is reading another dir: carry the store's index ourselves
		if data, err := os.ReadFile(filepath.Join(dir, NativeIndexFile)); err == nil && len(data) > 0 {
			parts = append(parts, CapChars("<!-- THIS REPO's MEMORY.md (native link missing or blocked) -->\n"+strings.TrimSpace(string(data)), indexCapChars))
		}
	}
	return CapChars(strings.Join(parts, "\n\n"), TotalCapChars)
}

// nativeLinked reports whether Claude Code's native memory dir for root is the symlink into dir.
func nativeLinked(root, dir string) bool {
	r := filepath.Clean(root)
	if top := mainCheckout(r); top != "" {
		r = top
	}
	cur, err := os.Readlink(projectsMemory(ProjectSlug(r)))
	return err == nil && paths.SamePath(cur, dir)
}

// sectionBody is the text under "## heading" up to the next "## " heading; "" when absent or empty.
// AST headings so a "## heading"-shaped line inside a fenced code block never ends the section early.
func sectionBody(text, heading string) string {
	doc := markdown.Parse([]byte(text))
	heads := level2Headings(doc.Body)
	for i, h := range heads {
		if !strings.EqualFold(h.title, heading) {
			continue
		}
		end := len(doc.Body)
		if i+1 < len(heads) {
			end = heads[i+1].lineStart
		}
		return strings.TrimSpace(string(doc.Body[h.lineEnd:end]))
	}
	return ""
}

// headingSpan is a level-2 ("## ") heading's title and the byte range of its own line, body-relative.
type headingSpan struct {
	title              string
	lineStart, lineEnd int
}

// level2Headings returns every "## " heading's title and line span (lineEnd after the trailing "\n").
// body is already front-matter-free (a Document.Body), so it parses directly with no re-stripping.
func level2Headings(body []byte) []headingSpan {
	var out []headingSpan
	root := markdown.ParseContext(body, parser.NewContext())
	_ = mdast.Walk(root, func(n mdast.Node, entering bool) (mdast.WalkStatus, error) { // walker never errors
		if !entering {
			return mdast.WalkContinue, nil
		}
		hn, ok := n.(*mdast.Heading)
		if !ok || hn.Level != 2 {
			return mdast.WalkContinue, nil
		}
		seg := hn.Lines().At(0)
		lineStart := seg.Start
		for lineStart > 0 && body[lineStart-1] != '\n' {
			lineStart--
		}
		lineEnd := seg.Stop
		for lineEnd < len(body) && body[lineEnd] != '\n' {
			lineEnd++
		}
		if lineEnd < len(body) {
			lineEnd++
		}
		out = append(out, headingSpan{title: string(seg.Value(body)), lineStart: lineStart, lineEnd: lineEnd})
		return mdast.WalkContinue, nil
	})
	return out
}

// capRows trims text to max chars at a whole-line boundary, pointing at the full file.
func capRows(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := strings.LastIndex(text[:max], "\n")
	if cut < 0 {
		cut = max
	}
	return text[:cut] + "\n- … (catalog truncated; read " + CatalogFile + " in the store)"
}
