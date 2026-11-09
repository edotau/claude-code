package memory

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory/bank"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	mdast "github.com/jeduden/mdsmith/pkg/goldmark/ast"
	"github.com/jeduden/mdsmith/pkg/goldmark/parser"
	"github.com/jeduden/mdsmith/pkg/markdown"
)

// Surface caps: a character budget under hooks' 9,500-char additionalContext cap
// (~1,000 left for the note). The index's slice is reserved so the current pair can never starve it.
const (
	totalCapChars = 8500
	indexCapChars = 2500
)

// Session-surface budgets: this repo's focus and the home bank's essentials, reserved so neither starves the other.
const (
	focusCapChars     = 2000
	homeFocusCapChars = 800
)

// SessionSurface is the session preload, capped at totalCapChars ("" when empty): root's focus sections plus the home bank's, and one pointer to the other
// repos (no catalog rows); Claude Code loads the repo's MEMORY.md (with its generated bank region) itself.
func SessionSurface(root string) string {
	dir := memstore.Dir(root)
	var parts []string
	if _, err := os.Stat(filepath.Join(memstore.StoreDir(), memstore.CatalogFile)); err == nil {
		parts = append(parts, "<!-- other repos: `claude-code memory search --all <terms>`; catalog "+filepath.Join(memstore.StoreDir(), memstore.CatalogFile)+" -->")
	}
	if HasBank(root) { // the header alone still names the bank when there is no focus text yet
		head := "<!-- THIS REPO (" + dir + ") — focus only; read the typed files on demand -->"
		if focus := repoFocus(dir); len(focus) > 0 {
			head += "\n" + strings.Join(focus, "\n\n")
		}
		parts = append(parts, bank.CapChars(head, focusCapChars))
	}
	if home := memstore.HomeBankDir(); !memstore.IsHomeBank(root) {
		if focus := repoFocus(home); len(focus) > 0 {
			parts = append(parts, bank.CapChars("<!-- HOME BANK ("+home+") — essentials -->\n"+strings.Join(focus, "\n\n"), homeFocusCapChars))
		}
	}
	if !nativeLinked(root, dir) { // Claude Code is reading another dir: carry the store's index ourselves
		if data, err := os.ReadFile(filepath.Join(dir, bank.NativeIndexFile)); err == nil && len(data) > 0 {
			parts = append(parts, bank.CapChars("<!-- THIS REPO's MEMORY.md (native link missing or blocked) -->\n"+strings.TrimSpace(string(data)), indexCapChars))
		}
	}
	return bank.CapChars(strings.Join(parts, "\n\n"), totalCapChars)
}

// repoFocus is a bank dir's injected sections: activeContext focus/blockers/next steps and progress "In Progress".
func repoFocus(dir string) []string {
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
	return focus
}

// nativeLinked reports whether Claude Code's native memory dir for root is the symlink into dir.
func nativeLinked(root, dir string) bool {
	r := filepath.Clean(root)
	if top := paths.MainCheckout(r, memstore.GitOut); top != "" {
		r = paths.CanonDeep(top)
	}
	target := memstore.NativeLinkTarget(memstore.ProjectsMemory(memstore.ProjectSlug(r)))
	return target != "" && paths.SamePath(target, dir)
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
		if hn.Lines().Len() == 0 { // a bare "## " has no title and no segment to locate it by
			return mdast.WalkContinue, nil
		}
		out = append(out, headingLineSpan(body, hn))
		return mdast.WalkContinue, nil
	})
	return out
}

func headingLineSpan(body []byte, hn *mdast.Heading) headingSpan {
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
	return headingSpan{title: string(seg.Value(body)), lineStart: lineStart, lineEnd: lineEnd}
}
