package bank

import (
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/jeduden/mdsmith/pkg/markdown"
)

// NativeDirs lists Claude Code's auto-memory dirs recall searches: the harness's own (user-wide facts), then root's.
func NativeDirs(root string) []string {
	out := []string{memstore.HomeBankDir()} // native shares the repo's store dir; the home tier holds the user-wide notes
	if d := memstore.Dir(root); !slices.Contains(out, d) {
		out = append(out, d)
	}
	return out
}

// Native auto-memory's MEMORY.md is injected EVERY session, so its size is a per-turn cost. SEARCHABLE.md
// holds pointer lines moved out of that injected core — recall finds them by content, never injected.
const (
	NativeIndexFile        = "MEMORY.md"     // the injected index of `- [Title](topic.md) — hook` lines
	NativeSearchableFile   = "SEARCHABLE.md" // pointer lines kept OUT of the injected index; recall finds the topics
	NativeArchiveIndexFile = "INDEX.md"      // struck pointer lines, recorded verbatim so restore is exact
	NativeCoreCap          = 20              // max pointer lines SplitNativeIndex leaves injected in MEMORY.md
)

// TopicFrontmatter splits a topic file into its name, description and body; false without a closed block and a name.
// The closing fence must start its own line (markdown.StripFrontMatter), so a name/description value that merely
// contains the literal bytes "---" can't be mistaken for it.
func TopicFrontmatter(raw string) (name, desc, body string, ok bool) {
	prefix, content := markdown.StripFrontMatter([]byte(raw))
	if prefix == nil {
		return "", "", "", false // an absent or unclosed block is body text, not frontmatter
	}
	for line := range strings.SplitSeq(string(prefix), "\n") {
		if v, found := strings.CutPrefix(line, "name:"); found {
			name = strings.Trim(strings.TrimSpace(v), `"'`)
		} else if v, found := strings.CutPrefix(line, "description:"); found {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if name == "" {
		return "", "", "", false
	}
	return name, desc, strings.TrimSpace(string(content)), true
}
