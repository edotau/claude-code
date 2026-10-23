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

// assignIDs stamps every unmarked block start in dir's typed files; returns how many it stamped.
func assignIDs(dir string, now time.Time) (int, error) {
	n := 0
	for name, prefix := range idFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		changed := false
		for i, l := range lines {
			if !strings.HasPrefix(l, prefix) || BlockID(l) != "" || strings.TrimSpace(strings.TrimPrefix(l, prefix)) == "" {
				continue
			}
			// i is in the hash so two identical lines in one settle pass never collide on the same ID.
			key := dir + "\x00" + name + "\x00" + strconv.Itoa(i) + "\x00" + l + "\x00" + now.Format(time.RFC3339Nano)
			sum := sha256.Sum256([]byte(key))
			lines[i] = l + " <!-- id:" + hex.EncodeToString(sum[:4]) + " -->"
			changed, n = true, n+1
		}
		if changed {
			if err := paths.AtomicWrite(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
				return n, err
			}
		}
	}
	return n, nil
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
