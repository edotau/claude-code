package memory

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/memory/bank"
)

func restoredIndex(b *nativeBatch, dir, healed string, lines []string) (string, error) {
	if len(indexLinks(healed)) >= bank.NativeCoreCap {
		// The full injected core leaves restored pointers recall-only and keeps its repair.
		searchable, err := readIfExists(filepath.Join(dir, bank.NativeSearchableFile))
		if err != nil {
			return "", err
		}
		b.put(bank.NativeSearchableFile, []byte(indexWithLines(string(searchable), lines)), searchable)
		lines = nil
	}
	return indexWithLines(healed, lines), nil
}
