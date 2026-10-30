package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"os"

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

func atomicWrite(p string, b []byte, mode os.FileMode) error { return paths.AtomicWrite(p, b, mode) }

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
	rollback := func(cause error) error {
		for i := len(touched) - 1; i >= 0; i-- {
			p := touched[i]
			s := before[p]
			var err error
			if s.exists {
				err = atomicWrite(p, s.data, s.mode)
			} else {
				err = os.Remove(p)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
			}
			if err != nil {
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
	for _, c := range changes {
		s := before[c.path]
		if (c.remove && !s.exists) || (!c.remove && s.exists && bytes.Equal(s.data, c.data)) {
			continue
		}
		touched = append(touched, c.path)
		var err error
		if c.remove {
			err = ops.remove(c.path)
		} else {
			mode := os.FileMode(0o600)
			if s.exists {
				mode = s.mode
			}
			err = ops.write(c.path, c.data, mode)
		}
		if err != nil {
			return rollback(fmt.Errorf("Desktop transaction %s: %w", c.path, err))
		}
	}
	return nil
}
