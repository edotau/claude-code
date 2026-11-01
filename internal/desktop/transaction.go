package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
)

type snapshot struct {
	data   []byte
	mode   os.FileMode
	exists bool
}
type change struct {
	path   string
	data   []byte
	remove bool
}
type fileOps struct {
	write  func(string, []byte, os.FileMode) error
	remove func(string) error
}

func capture(p string) (snapshot, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot{}, nil
	}
	if err != nil {
		return snapshot{}, err
	}
	if !fi.Mode().IsRegular() {
		return snapshot{}, fmt.Errorf("refusing non-regular Desktop file %s", p)
	}
	b, err := os.ReadFile(p)
	return snapshot{data: b, mode: fi.Mode().Perm(), exists: true}, err
}

func transact(changes []change, before map[string]snapshot, ops fileOps, library string) error {
	created := false
	if _, err := os.Stat(library); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(library, 0o700); err != nil {
			return err
		}
		created = true
	} else if err != nil {
		return err
	}
	touched := []string{}
	for _, c := range changes {
		s := before[c.path]
		if (c.remove && !s.exists) || (!c.remove && s.exists && bytes.Equal(s.data, c.data)) {
			continue
		}
		touched = append(touched, c.path)
		if err := applyChange(c, s, ops); err != nil {
			return rollback(fmt.Errorf("desktop transaction %s: %w", c.path, err), touched, before, created, library)
		}
	}
	return nil
}

// applyChange writes or removes c's file; a new file is 0600, an existing one keeps its mode.
func applyChange(c change, s snapshot, ops fileOps) error {
	if c.remove {
		return ops.remove(c.path)
	}
	mode := os.FileMode(0o600)
	if s.exists {
		mode = s.mode
	}
	return ops.write(c.path, c.data, mode)
}

// rollback restores touched in reverse, removes library when this run created it, and joins failures to cause.
func rollback(cause error, touched []string, before map[string]snapshot, created bool, library string) error {
	for _, p := range slices.Backward(touched) {
		if err := restoreFile(p, before[p]); err != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback %s: %w", p, err))
		}
	}
	if created {
		if err := os.Remove(library); err != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback directory: %w", err))
		}
	}
	return cause
}

// restoreFile puts p back to s; a file that did not exist is removed.
func restoreFile(p string, s snapshot) error {
	if s.exists {
		return paths.AtomicWrite(p, s.data, s.mode)
	}
	if err := os.Remove(p); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
