package bank

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memstore"
)

// FileKind classifies one entry of a memory dir; only KindTopic may be archived, restored or re-pointed.
type FileKind int

const (
	KindOther        FileKind = iota // non-regular (symlink/dir), non-.md, unknown dotfile — never mutated, never a topic
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

func (k FileKind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("FileKind(%d)", int(k))
}

// Archivable reports whether an archive/restore may move a file of this kind.
func (k FileKind) Archivable() bool { return k == KindTopic }

// ReservedNames maps every filename a memory dir reserves to its kind; bank files are reserved by name too.
var ReservedNames = reservedNameKinds()

func reservedNameKinds() map[string]FileKind {
	out := map[string]FileKind{
		NativeIndexFile:        KindNativeIndex,
		NativeSearchableFile:   KindSearchable,
		NativeArchiveIndexFile: KindArchiveIndex,
		memstore.CatalogFile:   KindCatalog,
	}
	for _, name := range bankFiles {
		out[name] = KindBank
	}
	return out
}

// ValidBareName rejects anything but a plain filename: callers join it onto a dir and move the result.
func ValidBareName(name string) error {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) ||
		strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%q: not a bare topic name", name)
	}
	return nil
}

// ClassifyMemoryFile is the directory-aware classification: name alone cannot tell a typed file from a topic.
func ClassifyMemoryFile(dir, name string) FileKind {
	if ValidBareName(name) != nil {
		return KindOther
	}
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return KindOther
	}
	if k, ok := ReservedNames[name]; ok {
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

func classifyTopicFile(path string) FileKind {
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
func reservedKindFold(name string) (FileKind, bool) {
	for reserved, k := range ReservedNames {
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

// The harness-generated region of MEMORY.md: its pointer lines are never counted, struck, moved or recorded.
// The region is reserved 40 lines / 4 KB including markers; the native loader caps the whole index at 200 lines / 25 KB.
const (
	bankRegionBegin     = "<!-- bank:begin"
	bankRegionEnd       = "<!-- bank:end -->"
	BankRegionMaxLines  = 40
	BankRegionMaxBytes  = 4 * 1024
	NativeIndexMaxLines = 200
	NativeIndexMaxBytes = 25 * 1024
)

// ErrBankRegion marks a MEMORY.md whose generated region cannot be located safely: a diagnostic, never a guess.
var ErrBankRegion = errors.New("malformed bank region")

// BankRegionBounds is ScanBankRegion for read-only callers; every rewrite goes through CheckBankRegion first,
// so a malformed region reads as absent here without ever being guessed at on disk.
func BankRegionBounds(text string) (start, end int) {
	start, end, _ = ScanBankRegion(text)
	return start, end
}

// ScanBankRegion is the byte range [start,end) of text's generated region, trailing newline included;
// (-1,-1,nil) when absent, ErrBankRegion for a lone marker, several of either, or end before begin.
func ScanBankRegion(text string) (start, end int, err error) {
	start, end = -1, -1
	begins, ends := 0, 0
	for off := 0; off < len(text); {
		lineEnd := len(text)
		if nl := strings.IndexByte(text[off:], '\n'); nl >= 0 {
			lineEnd = off + nl + 1
		}
		line := text[off:lineEnd]
		if strings.HasPrefix(line, bankRegionBegin) {
			begins++
			start = off
		}
		if strings.Contains(line, bankRegionEnd) {
			ends++
			end = lineEnd
		}
		off = lineEnd
	}
	return validateBankRegionBounds(start, end, begins, ends)
}

func validateBankRegionBounds(start, end, begins, ends int) (int, int, error) {
	if begins == 0 && ends == 0 {
		return -1, -1, nil
	}
	if begins != 1 || ends != 1 || end <= start {
		return -1, -1, fmt.Errorf("%w: %d begin / %d end markers, want one of each, begin first", ErrBankRegion, begins, ends)
	}
	return start, end, nil
}

// CheckBankRegion gates every MEMORY.md rewrite: malformed markers or a region past its reserve reject the mutation.
func CheckBankRegion(text string) error {
	start, end, err := ScanBankRegion(text)
	if err != nil || start < 0 {
		return err
	}
	region := text[start:end]
	if n := LineCount(region); n > BankRegionMaxLines || len(region) > BankRegionMaxBytes {
		return fmt.Errorf("%w: %d lines / %d bytes exceed the %d-line / %d-byte reserve",
			ErrBankRegion, n, len(region), BankRegionMaxLines, BankRegionMaxBytes)
	}
	return nil
}

// CheckNativeIndexLimits reports a MEMORY.md the native loader would truncate: the whole file, region included.
func CheckNativeIndexLimits(text string) error {
	if n := LineCount(text); n > NativeIndexMaxLines || len(text) > NativeIndexMaxBytes {
		return fmt.Errorf("%s: %d lines / %d bytes exceed the native load limit (%d lines / %d bytes)",
			NativeIndexFile, n, len(text), NativeIndexMaxLines, NativeIndexMaxBytes)
	}
	return nil
}

// OutsideBankRegion is text with its generated region cut out; unchanged when there is none.
func OutsideBankRegion(text string) string {
	start, end := BankRegionBounds(text)
	if start < 0 {
		return text
	}
	return text[:start] + text[end:]
}

// BankRegionLines is the set of line indexes (as strings.Split on "\n" numbers them) inside text's region;
// a malformed region is an error, never an empty set, so a rewrite cannot mistake its lines for pointers.
func BankRegionLines(text string) (map[int]bool, error) {
	in := map[int]bool{}
	start, end, err := ScanBankRegion(text)
	if err != nil || start < 0 {
		return in, err
	}
	off := 0
	for i, line := range strings.Split(text, "\n") {
		if off >= start && off < end {
			in[i] = true
		}
		off += len(line) + 1
	}
	return in, nil
}
