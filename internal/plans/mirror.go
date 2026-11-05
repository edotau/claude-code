package plans

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// ScratchDir holds one <draft-basename>.dest record per mirrored draft, pointing at its dated file.
func ScratchDir() string { return filepath.Join(paths.StateDir(), "plans-scratch") }

// DestRecordPath is the .dest record for the draft file with this base name.
func DestRecordPath(draftBase string) string {
	return filepath.Join(ScratchDir(), draftBase+".dest")
}

// ReadRecordedDest returns the dated file a draft was mirrored to, if one is recorded.
func ReadRecordedDest(draftBase string) (string, bool) {
	data, err := os.ReadFile(DestRecordPath(draftBase))
	if err != nil {
		return "", false
	}
	dest := strings.TrimSpace(string(data))
	return dest, dest != ""
}

// MirrorDests lists every unapproved draft's dated mirror, so no tick ever lands on a plan still being drafted.
func MirrorDests() []string {
	recs, _ := filepath.Glob(filepath.Join(ScratchDir(), "*.dest"))
	var out []string
	for _, r := range recs {
		if dest, ok := ReadRecordedDest(strings.TrimSuffix(filepath.Base(r), ".dest")); ok {
			out = append(out, dest)
		}
	}
	return out
}

// RecordDest records dest as the dated mirror of the draft with this base name.
func RecordDest(draftBase, dest string) error {
	if err := os.MkdirAll(ScratchDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(DestRecordPath(draftBase), []byte(dest), 0o600)
}
