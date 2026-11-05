package bank

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
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
	dir, err := memstore.EnsureRepoDir(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, file)
	var line, id string
	err = memstore.WithBankLock(memstore.BankLockWait, func() error {
		var werr error
		if file == "decisionLog.md" {
			line = "## " + now.Format("2006-01-02") + " — " + fact
			werr = prependBlock(path, file, line)
		} else {
			label := strings.ToUpper(kind[:1]) + kind[1:]
			line = "- " + label + ": " + fact
			werr = AppendBlocks(path, []string{line})
		}
		if werr != nil {
			return werr
		}
		// Stamp the id directly: SettleBank only runs assignIDs on the unified-store path,
		// and a fresh bank must still get an id back to round-trip through `memory recall`.
		if _, werr = assignIDs(dir, now); werr != nil {
			return werr
		}
		id, werr = lastStampedID(path, line, file == "decisionLog.md")
		return werr
	})
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
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // never rewrite the log from an unread file
	}
	header, blocks := EntryBlocks(base, string(data))
	blocks = append([]string{block}, blocks...)
	return paths.AtomicWrite(path, []byte(Rejoin(header, blocks)), 0o600)
}

// lastStampedID finds the id stamped on line's first line: the first exact match in a newest-first file, else the last.
func lastStampedID(path, line string, newestFirst bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	want, _, _ := strings.Cut(line, "\n")
	want = strings.TrimRight(want, " \t")
	lines := strings.Split(string(data), "\n")
	if !newestFirst {
		slices.Reverse(lines)
	}
	for _, ln := range lines {
		if id := BlockID(ln); id != "" && strings.TrimRight(StripID(ln), " \t") == want {
			return id, nil
		}
	}
	return "", errors.New("id not found after settle")
}
