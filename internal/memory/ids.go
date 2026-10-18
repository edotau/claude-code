package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// idMarker is a block's persisted ID: assigned once at settle, carried by the block through edits and archive moves.
var idMarker = regexp.MustCompile(`\s*<!-- id:([0-9a-f]{8}) -->`)

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
			sum := sha256.Sum256([]byte(dir + "\x00" + name + "\x00" + l + "\x00" + now.Format(time.RFC3339Nano)))
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

// ShowBlock finds the block carrying id anywhere in the store (archives included); returns its path and text.
func ShowBlock(id string) (string, string, error) {
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
		if err != nil || !strings.Contains(string(data), "<!-- id:"+id+" -->") {
			return nil
		}
		_, blocks := entryBlocks(logicalName(p), string(data))
		for _, b := range blocks {
			if strings.Contains(b, "<!-- id:"+id+" -->") {
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
