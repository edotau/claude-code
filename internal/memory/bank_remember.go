package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
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
	dir, err := EnsureRepoDir(root)
	if err != nil {
		return "", err
	}
	var id string
	err = WithBankLock(BankLockWait, func() error {
		var werr error
		if id, werr = writeRememberFact(dir, file, kind, fact, now); werr != nil {
			return werr
		}
		// Stamp ids directly: a fresh bank must get its id back to round-trip through `memory recall`.
		for name, prefix := range idFiles {
			if name == file { // already stamped in memory before its one write
				continue
			}
			if _, werr = assignFileIDs(dir, name, prefix, now); werr != nil {
				return werr
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := settleBank(root, now); err != nil {
		return "", err
	}
	return id, nil
}

// writeRememberFact adds fact to dir's file, stamps its id in memory, writes the file once, and returns the id.
func writeRememberFact(dir, file, kind, fact string, now time.Time) (string, error) {
	path := filepath.Join(dir, file)
	data, err := readFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err // never rewrite the file from an unread one
	}
	line, text := rememberedText(path, file, kind, fact, string(data), now)
	text, _ = stampIDs(dir, file, idFiles[file], text, now)
	id, err := lastStampedID(text, line, file == "decisionLog.md")
	if err != nil { // an unclosed fence above it or an id marker inside the fact: refuse before writing
		return "", fmt.Errorf("%s: the fact would get no id of its own: %w", file, err)
	}
	if err := paths.AtomicWrite(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// rememberedText is body with fact added: a dated heading on top of decisionLog.md, else a labeled bullet at the end.
func rememberedText(path, file, kind, fact, body string, now time.Time) (line, text string) {
	if strings.TrimSpace(body) == "" { // a missing typed file starts from its scaffold, never appendBlocks' archive header
		body = templates()[file]
	}
	if file == "decisionLog.md" {
		line = "## " + now.Format("2006-01-02") + " — " + fact
		return line, prependedText(file, body, line)
	}
	line = "- " + strings.ToUpper(kind[:1]) + kind[1:] + ": " + fact
	return line, appendedText(path, body, []string{line})
}

// prependedText is body with block inserted at the top of its entries — decisionLog.md is newest-first.
func prependedText(base, body, block string) string {
	header, blocks := entryBlocks(base, body)
	return rejoin(header, append([]string{block}, blocks...))
}

// lastStampedID finds the id stamped on line's first line in text: the first exact match in a newest-first file, else the last.
func lastStampedID(text, line string, newestFirst bool) (string, error) {
	want, _, _ := strings.Cut(line, "\n")
	want = strings.TrimRight(want, " \t")
	lines := strings.Split(text, "\n")
	if !newestFirst {
		slices.Reverse(lines)
	}
	for _, ln := range lines {
		if id := blockID(ln); id != "" && strings.TrimRight(stripID(ln), " \t") == want {
			return id, nil
		}
	}
	return "", errors.New("id not found after settle")
}
