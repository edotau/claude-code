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
			pair = append(pair, "<!-- "+name+" -->\n"+strings.TrimSpace(string(data)))
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

// globalSurface mirrors localSurface's durable half for the shared bank: index first, else promoted lessons.
func globalSurface() string {
	dir := GlobalDir()
	if idx := readIndex(dir); idx != "" {
		return "<!-- SHARED global bank index — full files under " + dir + " -->\n" + idx
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
