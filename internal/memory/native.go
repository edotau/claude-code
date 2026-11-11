package memory

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/jeduden/mdsmith/pkg/markdown"
)

// readIfExists reads path; an absent file is empty, any other failure is returned. A var so tests count reads.
var readIfExists = func(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// nativeDirs lists Claude Code's auto-memory dirs recall searches: the harness's own (user-wide facts), then root's.
func nativeDirs(root string) []string {
	out := []string{HomeBankDir()} // native shares the repo's store dir; the home tier holds the user-wide notes
	if d := Dir(root); !slices.Contains(out, d) {
		out = append(out, d)
	}
	return out
}

// Native auto-memory file names recognised when classifying a bank directory's files.
const (
	nativeIndexFile        = "MEMORY.md"     // the injected index of `- [Title](topic.md) — hook` lines
	nativeSearchableFile   = "SEARCHABLE.md" // overflow pointer lines kept out of the injected index
	nativeArchiveIndexFile = "INDEX.md"      // struck pointer lines recorded by the archive
)

// topicFrontmatter splits a topic file into its name, description and body; false without a closed block and a name.
// The closing fence must start its own line (markdown.StripFrontMatter), so a name/description value that merely
// contains the literal bytes "---" can't be mistaken for it.
func topicFrontmatter(raw string) (name, desc, body string, ok bool) {
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
