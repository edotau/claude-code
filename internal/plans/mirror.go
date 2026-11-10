package plans

import (
	"errors"
	"io/fs"
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
	ents, err := os.ReadDir(ScratchDir()) // not Glob: a metacharacter in the config path must not empty the list
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return allDatedPlans()
	}
	var out []string
	for _, e := range ents {
		base, ok := strings.CutSuffix(e.Name(), ".dest")
		if !ok {
			continue
		}
		dest, ok := ReadRecordedDest(base)
		if !ok {
			return allDatedPlans() // an unreadable record hides its mirror, so exclude every plan (fail closed)
		}
		out = append(out, dest)
	}
	return out
}

// allDatedPlans lists every plan file under the plans home, the fail-closed exclusion when a record is unreadable.
func allDatedPlans() []string {
	root := filepath.Join(paths.ConfigDir(), "plans")
	top, dirs := mdFiles(root)
	var sub []string
	for _, d := range dirs {
		files, _ := mdFiles(d)
		sub = append(sub, files...)
	}
	return append(top, sub...)
}

// mdFiles lists dir's *.md entries and its subdirectories' paths; an unreadable dir has neither.
func mdFiles(dir string) (md, dirs []string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			dirs = append(dirs, p)
		case strings.HasSuffix(e.Name(), ".md"):
			md = append(md, p)
		}
	}
	return md, dirs
}

// RecordDest records dest as the dated mirror of the draft with this base name.
func RecordDest(draftBase, dest string) error {
	if err := os.MkdirAll(ScratchDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(DestRecordPath(draftBase), []byte(dest), 0o600)
}
