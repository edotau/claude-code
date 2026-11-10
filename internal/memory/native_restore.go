package memory

import (
	"path/filepath"
)

// restoredIndex fills the injected core up to NativeCoreCap; restored pointers beyond it go to SEARCHABLE.md, recall-only.
func restoredIndex(b *nativeBatch, dir, healed string, lines []string) (string, error) {
	room := max(NativeCoreCap-len(indexLinks(healed)), 0)
	var overflow []string
	if len(lines) > room {
		lines, overflow = lines[:room], lines[room:]
	}
	if len(overflow) > 0 {
		searchable, err := readIfExists(filepath.Join(dir, NativeSearchableFile))
		if err != nil {
			return "", err
		}
		b.put(NativeSearchableFile, []byte(indexWithLines(string(searchable), overflow)), searchable)
	}
	return indexWithLines(healed, lines), nil
}
