package memory

import (
	"os"
	"strings"
)

// reservedNames is every filename a memory dir reserves; the six typed bank files are reserved by name too.
var reservedNames = reservedNameSet()

func reservedNameSet() map[string]bool {
	out := map[string]bool{
		nativeIndexFile:        true,
		nativeSearchableFile:   true,
		nativeArchiveIndexFile: true,
		catalogFile:            true,
	}
	for _, name := range bankFiles {
		out[name] = true
	}
	return out
}

// reservedKindFold reports whether name is reserved under case folding ("activecontext.md" opens activeContext.md).
func reservedKindFold(name string) bool {
	for reserved := range reservedNames {
		if strings.EqualFold(reserved, name) {
			return true
		}
	}
	return false
}

// hasExactEntry reports whether dir lists name in exactly this spelling; false means Lstat hit a case-folded twin.
func hasExactEntry(dir, name string) bool {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() == name {
			return true
		}
	}
	return false
}

// frontmatterValue is key's value inside a closed leading `---` block; "" when absent, unclosed, or a list/map.
func frontmatterValue(raw, key string) string {
	lines := strings.Split(raw, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	value := ""
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return value
		}
		if v, found := strings.CutPrefix(line, key+":"); found && value == "" {
			value = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
