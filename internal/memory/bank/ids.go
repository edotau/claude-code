package bank

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// idMarker is a block's persisted ID: assigned once at settle, carried by the block through edits and archive moves.
var idMarker = regexp.MustCompile(`\s*<!-- id:([0-9a-f]{8}) -->`)

// contentRefPrefix leads a derived (unpersisted) block reference; 'z' is never a hex digit, so it can
// never collide with a real id marker — `show`/`recall --load` tell the two apart by this prefix.
const contentRefPrefix = "z"

// ContentRef derives a stable, show-able reference for a block with no persisted id marker:
// the prefix plus the first 4 bytes of sha256 over its whitespace-normalized text.
func ContentRef(block string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(block), " ")))
	return contentRefPrefix + hex.EncodeToString(sum[:4])
}

// isHex reports whether s is a non-empty run of lowercase hex digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// IdFiles are the typed files whose blocks get IDs, with the prefix that starts a block.
var IdFiles = map[string]string{"decisionLog.md": "## ", "sessionHistory.md": "## ", "conventions.md": "- "}

// BlockID is the persisted ID on line ("" when it has none).
func BlockID(line string) string {
	if m := idMarker.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// StripID removes the ID marker for display.
func StripID(s string) string { return idMarker.ReplaceAllString(s, "") }

// assignIDs stamps every unmarked block in dir's typed files and collapses stacked markers to the oldest (the
// last in the block); a block is judged whole because the formatter may wrap its marker onto a later line.
// Returns how many blocks it stamped.
func assignIDs(dir string, now time.Time) (int, error) {
	n := 0
	for name, prefix := range IdFiles {
		stamped, err := assignFileIDs(dir, name, prefix, now)
		n += stamped
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// assignFileIDs is assignIDs for one typed file; a missing file is skipped.
func assignFileIDs(dir, name, prefix string, now time.Time) (int, error) {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	lines := strings.Split(string(data), "\n")
	starts := blockStarts(lines, prefix)
	out := make([]string, 0, len(lines))
	out = append(out, lines[:min(len(lines), first(starts, len(lines)))]...)
	for k, i := range starts {
		end := len(lines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		block, stamped := settleBlockID(dir, name, i, lines[i:end], now)
		n += stamped
		out = append(out, block...)
	}
	if res := strings.Join(out, "\n"); res != string(data) {
		if err := paths.AtomicWrite(path, []byte(res), 0o600); err != nil {
			return n, err
		}
	}
	return n, nil
}

// blockStarts returns the index of each non-empty prefix line outside code fences.
func blockStarts(lines []string, prefix string) []int {
	var starts []int
	inFence := false
	for i, l := range lines {
		if !inFence && strings.HasPrefix(l, prefix) && strings.TrimSpace(strings.TrimPrefix(l, prefix)) != "" {
			starts = append(starts, i)
		}
		if fenceToggle(l) {
			inFence = !inFence
		}
	}
	return starts
}

func first(xs []int, def int) int {
	if len(xs) == 0 {
		return def
	}
	return xs[0]
}

// fencedLines flags each line that is a code fence or sits inside one; markers there are examples, not ids.
func fencedLines(block []string) []bool {
	out := make([]bool, len(block))
	in := false
	for i, l := range block {
		toggle := fenceToggle(l)
		out[i] = in || toggle
		if toggle {
			in = !in
		}
	}
	return out
}

// settleBlockID leaves exactly one id marker, on the block's first line: the oldest it already carries (the last,
// since settle stacks newer ones above), else a fresh one. Fenced lines are examples and are never touched.
func settleBlockID(dir, name string, idx int, block []string, now time.Time) ([]string, int) {
	fenced := fencedLines(block)
	keepLine, keepSpan, total := lastMarker(block, fenced)
	if total == 0 {
		return append([]string{block[0] + freshID(dir, name, idx, block[0], now)}, block[1:]...), 1
	}
	if total == 1 && keepLine == 0 {
		return block, 0
	}
	kept := " " + strings.TrimSpace(block[keepLine][keepSpan[0]:keepSpan[1]])
	out := dropMarkers(block, fenced, keepLine, keepSpan)
	if keepLine != 0 {
		out[0] = strings.TrimRight(out[0], " \t") + kept
	}
	return out, 0
}

// dropMarkers removes every id marker but the survivor; a non-first line left blank by it disappears.
func dropMarkers(block []string, fenced []bool, keepLine int, keepSpan []int) []string {
	out := make([]string, 0, len(block))
	for i, l := range block {
		if !fenced[i] && idMarker.MatchString(l) {
			res := stripMarkers(l, i == 0 && i == keepLine, keepSpan) // a first-line survivor stays where it is
			if strings.TrimSpace(res) == "" && i != 0 {
				continue
			}
			l = res
		}
		out = append(out, l)
	}
	return out
}

// lastMarker finds the last unfenced id marker in block: its line, its span and how many markers there are.
func lastMarker(block []string, fenced []bool) (keepLine int, keepSpan []int, total int) {
	keepLine = -1
	for i, l := range block {
		if fenced[i] {
			continue
		}
		for _, sp := range idMarker.FindAllStringIndex(l, -1) {
			keepLine, keepSpan, total = i, sp, total+1
		}
	}
	return keepLine, keepSpan, total
}

// freshID is the new ` <!-- id:xxxx -->` suffix for a block; idx is in the hash so identical lines never collide.
func freshID(dir, name string, idx int, line string, now time.Time) string {
	key := dir + "\x00" + name + "\x00" + strconv.Itoa(idx) + "\x00" + line + "\x00" + now.Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(key))
	return " <!-- id:" + hex.EncodeToString(sum[:4]) + " -->"
}

// stripMarkers removes every id marker from l, except the one at keepSpan when keepHere.
func stripMarkers(l string, keepHere bool, keepSpan []int) string {
	var b strings.Builder
	prev := 0
	for _, sp := range idMarker.FindAllStringIndex(l, -1) {
		b.WriteString(l[prev:sp[0]])
		if keepHere && sp[0] == keepSpan[0] {
			b.WriteString(l[sp[0]:sp[1]])
		}
		prev = sp[1]
	}
	b.WriteString(l[prev:])
	return b.String()
}

// ErrNoBlock is returned when no block in the store carries the requested ID.
var ErrNoBlock = errors.New("no memory block with that id")

// ShowBlock finds the block carrying ref anywhere in the store (archives included); ref is either a
// persisted `<!-- id:xxxx -->` marker or a ContentRef for a block that never got one. Returns path and text.
func ShowBlock(ref string) (string, string, error) {
	contentRef := strings.HasPrefix(ref, contentRefPrefix) && isHex(strings.TrimPrefix(ref, contentRefPrefix))
	var foundPath, foundBlock string
	err := filepath.WalkDir(memstore.StoreDir(), func(p string, d fs.DirEntry, err error) error {
		if foundPath != "" { // already matched: a later unreadable entry must not discard the match
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) { // no store yet: nothing to find
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		if b, ok := blockWithRef(p, ref, contentRef); ok {
			foundPath, foundBlock = p, b
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if foundPath == "" {
		return "", "", ErrNoBlock
	}
	return foundPath, foundBlock, nil
}

// blockWithRef returns the last block in file p carrying ref (an id marker, or a ContentRef when contentRef).
func blockWithRef(p, ref string, contentRef bool) (found string, ok bool) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	if !contentRef && !strings.Contains(string(data), "<!-- id:"+ref+" -->") {
		return "", false
	}
	_, blocks := EntryBlocks(logicalName(p), string(data))
	if contentRef { // native topics and knowledge notes are indexed whole, under their own grammar
		base := filepath.Base(p)
		blocks = append(blocks, ParseBlocks(SegNative, base, data)...)
		blocks = append(blocks, ParseBlocks(SegKnowledge, base, data)...)
	}
	for _, b := range blocks {
		matches := strings.Contains(b, "<!-- id:"+ref+" -->")
		if contentRef {
			matches = ContentRef(b) == ref
		}
		if matches {
			found, ok = b, true
		}
	}
	return found, ok
}

// logicalName maps an archive file (decisionLog-2026-10.md) back to its typed file's grammar.
func logicalName(path string) string {
	base := filepath.Base(path)
	for name := range IdFiles {
		if stem := strings.TrimSuffix(name, ".md"); base == name || regexp.MustCompile(`^`+stem+`-\d{4}-\d{2}\.md$`).MatchString(base) {
			return name
		}
	}
	return base
}
