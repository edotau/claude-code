package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// RememberTypes maps `memory remember --type` to the bank file it writes: a "project" fact is a
// dated decisionLog.md entry (newest first); every other kind is a conventions.md bullet.
var RememberTypes = map[string]string{
	"project":   "decisionLog.md",
	"user":      "conventions.md",
	"feedback":  "conventions.md",
	"reference": "conventions.md",
}

// Remember writes fact into root's bank under kind's file, settles to stamp a fresh id, and
// returns that id (so a caller can round-trip it through `memory recall`).
func Remember(root, kind, fact string, now time.Time) (string, error) {
	file, ok := RememberTypes[kind]
	if !ok {
		return "", fmt.Errorf("--type %q: want user, feedback, project, or reference", kind)
	}
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return "", errors.New("empty fact")
	}
	dir := Dir(root)
	path := filepath.Join(dir, file)
	var line string
	var writeErr error
	if file == "decisionLog.md" {
		line = "## " + now.Format("2006-01-02") + " — " + fact
		writeErr = prependBlock(path, file, line)
	} else {
		label := strings.ToUpper(kind[:1]) + kind[1:]
		line = "- " + label + ": " + fact
		writeErr = appendBlocks(path, []string{line})
	}
	if writeErr != nil {
		return "", writeErr
	}
	// Stamp the id directly: SettleBank only runs assignIDs on the post-cutover unified-store path,
	// and a fresh/legacy bank must still get an id back to round-trip through `memory recall`.
	if _, err := assignIDs(dir, now); err != nil {
		return "", err
	}
	id, err := lastStampedID(path, line)
	if err != nil {
		return "", err
	}
	if _, err := SettleBank(root, now); err != nil {
		return "", err
	}
	return id, nil
}

// prependBlock inserts block at the top of path's entries — decisionLog.md is newest-first.
func prependBlock(path, base, block string) error {
	data, _ := os.ReadFile(path)
	header, blocks := entryBlocks(base, string(data))
	blocks = append([]string{block}, blocks...)
	return paths.AtomicWrite(path, []byte(rejoin(header, blocks)), 0o600)
}

// lastStampedID finds the id settle just stamped onto the line it wrote (matched by exact prefix).
func lastStampedID(path, line string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for ln := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(ln, line) {
			if id := BlockID(ln); id != "" {
				return id, nil
			}
		}
	}
	return "", errors.New("id not found after settle")
}
