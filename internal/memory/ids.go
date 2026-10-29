package memory

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

// idFiles are the typed files whose blocks get IDs, with the prefix that starts a block.
var idFiles = map[string]string{"decisionLog.md": "## ", "sessionHistory.md": "## ", "conventions.md": "- "}

// BlockID is the persisted ID on line ("" when it has none).
func BlockID(line string) string {
	if m := idMarker.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// stripID removes the ID marker for display.
func stripID(s string) string { return idMarker.ReplaceAllString(s, "") }

// assignIDs stamps every unmarked block in dir's typed files and collapses stacked markers to the oldest (the
// last in the block); a block is judged whole because the formatter may wrap its marker onto a later line.
// Returns how many blocks it stamped.
func assignIDs(dir string, now time.Time) (int, error) {
	n := 0
	for name, prefix := range idFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
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
	}
	return n, nil
}

func first(xs []int, def int) int {
	if len(xs) == 0 {
		return def
	}
	return xs[0]
}

// settleBlockID leaves exactly one id marker in a block: the oldest it already carries, else a fresh one on line 0.
func settleBlockID(dir, name string, idx int, block []string, now time.Time) ([]string, int) {
	total := 0
	for _, l := range block {
		total += len(idMarker.FindAllStringIndex(l, -1))
	}
	if total == 0 {
		// idx is in the hash so two identical lines in one settle pass never collide on the same ID.
		key := dir + "\x00" + name + "\x00" + strconv.Itoa(idx) + "\x00" + block[0] + "\x00" + now.Format(time.RFC3339Nano)
		sum := sha256.Sum256([]byte(key))
		return append([]string{block[0] + " <!-- id:" + hex.EncodeToString(sum[:4]) + " -->"}, block[1:]...), 1
	}
	if total == 1 {
		return block, 0
	}
	seen := 0
	out := make([]string, 0, len(block))
	for _, l := range block {
		spans := idMarker.FindAllStringIndex(l, -1)
		if len(spans) == 0 {
			out = append(out, l)
			continue
		}
		var b strings.Builder
		prev := 0
		for _, sp := range spans {
			seen++
			b.WriteString(l[prev:sp[0]])
			if seen == total { // keep only the last marker
				b.WriteString(l[sp[0]:sp[1]])
			}
			prev = sp[1]
		}
		b.WriteString(l[prev:])
		if res := b.String(); strings.TrimSpace(res) != "" {
			out = append(out, res)
		}
	}
	return out, 0
}

// ErrNoBlock is returned when no block in the store carries the requested ID.
var ErrNoBlock = errors.New("no memory block with that id")

// ShowBlock finds the block carrying ref anywhere in the store (archives included); ref is either a
// persisted `<!-- id:xxxx -->` marker or a ContentRef for a block that never got one. Returns path and text.
func ShowBlock(ref string) (string, string, error) {
	contentRef := strings.HasPrefix(ref, contentRefPrefix) && isHex(strings.TrimPrefix(ref, contentRefPrefix))
	var foundPath, foundBlock string
	_ = filepath.WalkDir(StoreDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || foundPath != "" {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if !contentRef && !strings.Contains(string(data), "<!-- id:"+ref+" -->") {
			return nil
		}
		_, blocks := entryBlocks(logicalName(p), string(data))
		for _, b := range blocks {
			if contentRef {
				if ContentRef(b) == ref {
					foundPath, foundBlock = p, b
				}
				continue
			}
			if strings.Contains(b, "<!-- id:"+ref+" -->") {
				foundPath, foundBlock = p, b
			}
		}
		return nil
	})
	if foundPath == "" {
		return "", "", ErrNoBlock
	}
	return foundPath, foundBlock, nil
}

// logicalName maps an archive file (decisionLog-2026-10.md) back to its typed file's grammar.
func logicalName(path string) string {
	base := filepath.Base(path)
	for name := range idFiles {
		if strings.HasPrefix(base, strings.TrimSuffix(name, ".md")) {
			return name
		}
	}
	return base
}
