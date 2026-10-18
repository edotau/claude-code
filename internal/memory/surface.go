package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Surface caps: per-surface byte guards, then a character budget under hooks' 9,500-char additionalContext cap
// (~1,000 left for the note). The index's slice is reserved so the current pair can never starve it.
const (
	localCapBytes    = 20 << 10
	globalCapBytes   = 24 << 10
	TotalCapChars    = 8500
	indexCapChars    = 2500
	pairCapChars     = TotalCapChars - indexCapChars - 2
	localDecisionMax = 3
)

const truncatedNotice = "\n<!-- (surface truncated) -->"

// SessionSurface composes root's local bank surface plus the global one, capped at TotalCapChars; "" when empty.
func SessionSurface(root string) string {
	if CutOver() {
		return unifiedSurface(root)
	}
	var parts []string
	if local := capBytes(localSurface(Dir(root), root), localCapBytes); local != "" {
		parts = append(parts, local)
	}
	if !IsGlobal(root) {
		if shared := capBytes(globalSurface(), globalCapBytes); shared != "" {
			parts = append(parts, shared)
		}
	}
	return CapChars(strings.Join(parts, "\n\n"), TotalCapChars)
}

// localSurface: activeContext+progress in full first, then index.md, else newest decisions + newest history entry.
func localSurface(dir, root string) string {
	var pair []string
	for _, name := range []string{"activeContext.md", "progress.md"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil && len(data) > 0 {
			body := string(data)
			if name == "activeContext.md" {
				body = dropParked(body)
			}
			pair = append(pair, "<!-- "+name+" -->\n"+strings.TrimSpace(body))
		}
	}
	var durable string
	if idx := readIndex(dir); idx != "" {
		durable = "<!-- LOCAL memory bank index (durable: decisionLog/sessionHistory/conventions) — full files under " + dir + " -->\n" + idx
	} else {
		var body []string
		if blocks := NewestHeadingBlocks(filepath.Join(dir, "decisionLog.md"), localDecisionMax); len(blocks) > 0 {
			body = append(body, "<!-- decisionLog.md (newest entries) -->\n"+strings.Join(blocks, "\n\n"))
		}
		if blocks := NewestHeadingBlocks(filepath.Join(dir, "sessionHistory.md"), 1); len(blocks) > 0 {
			body = append(body, "<!-- sessionHistory.md (newest entry) -->\n"+blocks[0])
		}
		if len(body) > 0 {
			durable = "<!-- LOCAL working bank (index.md missing — run `claude-code memory index --repo " + root + "`; dir: " + dir + ") -->\n" + strings.Join(body, "\n\n")
		}
	}
	var parts []string
	if len(pair) > 0 {
		parts = append(parts, CapChars("<!-- LOCAL current (activeContext + progress) — full files under "+dir+" -->\n"+strings.Join(pair, "\n\n"), pairCapChars))
	}
	if durable != "" {
		parts = append(parts, CapChars(durable, indexCapChars))
	}
	return strings.Join(parts, "\n\n")
}

// globalSurface points at an indexed shared bank (memory-recall searches it per prompt), else injects promoted lessons.
func globalSurface() string {
	dir := GlobalDir()
	if readIndex(dir) != "" {
		return "<!-- SHARED global bank: " + dir + " — recalled per prompt by the memory-recall hook; " +
			"claude-code memory search \"<q>\" for more -->"
	}
	if shared := ComposePromotedSurface(dir, globalCapBytes); shared != "" {
		return "<!-- SHARED global bank (index.md missing — run `claude-code memory index`; dir: " + dir + ") -->\n" + shared
	}
	return ""
}

func readIndex(dir string) string {
	data, _ := os.ReadFile(filepath.Join(dir, BankIndexFile))
	return strings.TrimSpace(string(data))
}

// ComposePromotedSurface renders the global conventions/decisionLog newest-first, whole blocks within budget bytes.
func ComposePromotedSurface(dir string, budget int) string {
	var sections []string
	used := 0
	for _, name := range []string{"conventions.md", "decisionLog.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		_, blocks := entryBlocks(name, string(data))
		if len(blocks) == 0 {
			continue
		}
		head := "<!-- global/" + name + " (newest first) -->"
		used += len(head)
		var kept []string
		omitted := 0
		for i := len(blocks) - 1; i >= 0; i-- {
			cost := len(blocks[i]) + 2
			if used+cost > budget {
				omitted = i + 1
				break
			}
			kept = append(kept, blocks[i])
			used += cost
		}
		if omitted > 0 {
			kept = append(kept, fmt.Sprintf("<!-- %d older entries omitted; see memory/index.md -->", omitted))
		}
		sections = append(sections, head+"\n"+strings.Join(kept, "\n\n"))
	}
	return strings.Join(sections, "\n\n")
}

// capBytes cuts s to max bytes at the last whole line (rune-safe fallback) with a marker.
func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	} else {
		cut = strings.ToValidUTF8(cut, "")
	}
	return cut + truncatedNotice
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

// dropParked removes activeContext's "## Parked" section from the injected surface: parked items are not
// this session's work, and memory-recall still finds each one by topic (it indexes the file per bullet).
func dropParked(body string) string {
	lines := strings.Split(body, "\n")
	var out []string
	parked, items := false, 0
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			parked = strings.TrimSpace(ln) == "## Parked"
			if parked {
				continue
			}
		}
		if parked {
			if strings.HasPrefix(ln, "- ") {
				items++
			}
			continue
		}
		out = append(out, ln)
	}
	if items > 0 {
		out = append(out, fmt.Sprintf("\n<!-- %d parked item(s) omitted; memory-recall surfaces them by topic -->", items))
	}
	return strings.Join(out, "\n")
}

// Unified-store budgets (plan §3.4): the catalog and the focus slices are reserved so neither starves the other.
const (
	catalogCapChars = 2500
	focusCapChars   = 2000
)

// unifiedSurface is the post-cutover preload: the catalog router plus root's focus sections; Claude Code loads
// the repo's MEMORY.md (with its generated bank region) itself, so no index is injected here.
func unifiedSurface(root string) string {
	dir := Dir(root)
	var parts []string
	if data, err := os.ReadFile(filepath.Join(StoreDir(), CatalogFile)); err == nil {
		parts = append(parts, "<!-- MEMORY CATALOG (start here; "+StoreDir()+") -->\n"+capRows(strings.TrimSpace(string(data)), catalogCapChars))
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
	return CapChars(strings.Join(parts, "\n\n"), TotalCapChars)
}

// sectionBody is the text under "## heading" up to the next "## " heading; "" when absent or empty.
func sectionBody(text, heading string) string {
	var out []string
	in := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "## ") {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(l, "## ")), heading)
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
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
