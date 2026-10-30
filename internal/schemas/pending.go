package schemas

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// Stale is one file holding data older than its format's current version.
type Stale struct {
	Format  string
	Path    string
	From    int // oldest version found in the file
	To      int
	Records int // stale records (lines for a log, 1 otherwise)
	Rewrite bool
	Err     string `json:",omitempty"`
}

// docs splits a file into JSON documents: one per line for logs, the whole file otherwise.
func docs(f Format, data []byte) [][]byte {
	if f.Kind != Log {
		return [][]byte{data}
	}
	var out [][]byte
	for l := range bytes.SplitSeq(data, []byte("\n")) {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func onDisk(f Format) bool {
	return f.Glob != nil && f.Kind == Record || f.Kind == Log
}

func scan(f Format) []Stale {
	if !onDisk(f) {
		return nil
	}
	files, _ := filepath.Glob(f.Glob())
	var out []Stale
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := Stale{Format: f.Name, Path: p, To: f.Version, Rewrite: f.Rewrite, From: f.Version}
		for _, d := range docs(f, data) {
			v, err := peek(d)
			if err != nil || v >= f.Version {
				continue
			}
			s.Records++
			s.From = min(s.From, v)
		}
		if s.Records > 0 {
			out = append(out, s)
		}
	}
	return out
}

// Pending lists Record and Log files older than their registered version; a cache mismatch is a miss, not stale.
// A row with Rewrite false is read via migration (informational).
func Pending() []Stale {
	var out []Stale
	for _, f := range All() {
		out = append(out, scan(f)...)
	}
	return out
}

// MigrateAll rewrites stale files of Rewrite rows under a file lock; dryRun only reports. Other rows migrate on read.
func MigrateAll(dryRun bool) ([]Stale, error) {
	var done []Stale
	var firstErr error
	for _, f := range All() {
		if !f.Rewrite {
			continue
		}
		files := map[int]int{} // from -> files rewritten
		for _, s := range scan(f) {
			if !dryRun {
				if err := rewrite(f, &s); err != nil {
					s.Err = err.Error()
					if firstErr == nil {
						firstErr = err
					}
				} else {
					files[s.From]++
				}
			}
			done = append(done, s)
		}
		for from, n := range files {
			LogMigration(f.Name, from, f.Version, n)
		}
	}
	return done, firstErr
}

func rewrite(f Format, s *Stale) error {
	return paths.WithFileLock(s.Path, func() error {
		data, err := os.ReadFile(s.Path) // re-read under the lock
		if err != nil {
			return err
		}
		var out [][]byte
		n := 0
		for _, d := range docs(f, data) {
			if v, perr := peek(d); perr == nil && v < f.Version {
				m, _, err := upgrade(f, d)
				if err != nil {
					return err
				}
				d = m
				n++
			}
			out = append(out, d)
		}
		if f.Kind != Log {
			if err := paths.AtomicWrite(s.Path, out[0], 0o600); err != nil {
				return err
			}
		} else if err := paths.AtomicWrite(s.Path, append(bytes.Join(out, []byte("\n")), '\n'), 0o600); err != nil {
			return err
		}
		s.Records = n
		return nil
	})
}
