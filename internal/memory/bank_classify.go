package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fileKind classifies one entry of a memory dir; only KindTopic may be archived, restored or re-pointed.
type fileKind int

const (
	KindOther        fileKind = iota // non-regular (symlink/dir), non-.md, unknown dotfile — never mutated, never a topic
	KindTopic                        // native topic file: the only archivable kind
	KindBank                         // one of the six typed files BY NAME, or any .md whose frontmatter says `harness_kind: bank`
	KindNativeIndex                  // MEMORY.md
	KindSearchable                   // SEARCHABLE.md
	KindArchiveIndex                 // INDEX.md
	KindCatalog                      // index.md — reserved in every memory dir (the global catalog at the store root)
)

var kindNames = [...]string{
	KindOther:        "not a regular .md file",
	KindTopic:        "topic",
	KindBank:         "typed bank file",
	KindNativeIndex:  "native index",
	KindSearchable:   "searchable pointer file",
	KindArchiveIndex: "archive index",
	KindCatalog:      "catalog",
}

func (k fileKind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("fileKind(%d)", int(k))
}

// Archivable reports whether an archive/restore may move a file of this kind.
func (k fileKind) Archivable() bool { return k == KindTopic }

// reservedNames maps every filename a memory dir reserves to its kind; bank files are reserved by name too.
var reservedNames = reservedNameKinds()

func reservedNameKinds() map[string]fileKind {
	out := map[string]fileKind{
		NativeIndexFile:        KindNativeIndex,
		NativeSearchableFile:   KindSearchable,
		NativeArchiveIndexFile: KindArchiveIndex,
		CatalogFile:            KindCatalog,
	}
	for _, name := range bankFiles {
		out[name] = KindBank
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
		return KindOther
	}
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return KindOther
	}
	if k, ok := reservedNames[name]; ok {
		return k
	}
	if k, ok := reservedKindFold(name); ok && !hasExactEntry(dir, name) {
		return k
	}
	if !strings.HasSuffix(name, ".md") {
		return KindOther
	}
	return classifyTopicFile(path)
}

func classifyTopicFile(path string) fileKind {
	raw, err := os.ReadFile(path)
	if err != nil {
		return KindOther
	}
	if frontmatterValue(string(raw), "harness_kind") == "bank" {
		return KindBank
	}
	return KindTopic
}

// reservedKindFold is name's reserved kind under case folding: "activecontext.md" opens activeContext.md on APFS.
func reservedKindFold(name string) (fileKind, bool) {
	for reserved, k := range reservedNames {
		if strings.EqualFold(reserved, name) {
			return k, true
		}
	}
	return KindOther, false
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
