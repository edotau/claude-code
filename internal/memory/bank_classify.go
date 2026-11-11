package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fileKind classifies one entry of a memory dir; only kindTopic may be re-pointed.
type fileKind int

const (
	kindOther        fileKind = iota // non-regular (symlink/dir), non-.md, unknown dotfile — never mutated, never a topic
	kindTopic                        // native topic file
	kindBank                         // one of the six typed files BY NAME, or any .md whose frontmatter says `harness_kind: bank`
	kindNativeIndex                  // MEMORY.md
	kindSearchable                   // SEARCHABLE.md
	kindArchiveIndex                 // INDEX.md
	kindCatalog                      // index.md — reserved in every memory dir (the global catalog at the store root)
)

var kindNames = [...]string{
	kindOther:        "not a regular .md file",
	kindTopic:        "topic",
	kindBank:         "typed bank file",
	kindNativeIndex:  "native index",
	kindSearchable:   "searchable pointer file",
	kindArchiveIndex: "archive index",
	kindCatalog:      "catalog",
}

func (k fileKind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("fileKind(%d)", int(k))
}

// reservedNames maps every filename a memory dir reserves to its kind; bank files are reserved by name too.
var reservedNames = reservedNameKinds()

func reservedNameKinds() map[string]fileKind {
	out := map[string]fileKind{
		nativeIndexFile:        kindNativeIndex,
		nativeSearchableFile:   kindSearchable,
		nativeArchiveIndexFile: kindArchiveIndex,
		catalogFile:            kindCatalog,
	}
	for _, name := range bankFiles {
		out[name] = kindBank
	}
	return out
}

// validBareName rejects anything but a plain filename: callers join it onto a dir and move the result.
func validBareName(name string) error {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) ||
		strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%q: not a bare topic name", name)
	}
	return nil
}

// classifyMemoryFile is the directory-aware classification: name alone cannot tell a typed file from a topic.
func classifyMemoryFile(dir, name string) fileKind {
	if validBareName(name) != nil {
		return kindOther
	}
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return kindOther
	}
	if k, ok := reservedNames[name]; ok {
		return k
	}
	if k, ok := reservedKindFold(name); ok && !hasExactEntry(dir, name) {
		return k
	}
	if !strings.HasSuffix(name, ".md") {
		return kindOther
	}
	return classifyTopicFile(path)
}

func classifyTopicFile(path string) fileKind {
	raw, err := os.ReadFile(path)
	if err != nil {
		return kindOther
	}
	if frontmatterValue(string(raw), "harness_kind") == "bank" {
		return kindBank
	}
	return kindTopic
}

// reservedKindFold is name's reserved kind under case folding: "activecontext.md" opens activeContext.md on APFS.
func reservedKindFold(name string) (fileKind, bool) {
	for reserved, k := range reservedNames {
		if strings.EqualFold(reserved, name) {
			return k, true
		}
	}
	return kindOther, false
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
