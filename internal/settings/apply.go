package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// SettingsFile is the live Claude Code settings.json under the config dir.
func SettingsFile() string { return filepath.Join(paths.ConfigDir(), "settings.json") }

// SeedSettings writes Render (through transform when non-nil) to file only when file does not exist yet;
// an existing file is never read or written.
func SeedSettings(file string, transform func([]byte) ([]byte, error)) (seeded bool, err error) {
	err = paths.WithFileLock(file, func() error {
		if _, serr := os.Stat(file); !errors.Is(serr, fs.ErrNotExist) {
			return serr
		}
		data, err := Render()
		if err == nil && transform != nil {
			data, err = transform(data)
		}
		if err != nil {
			return err
		}
		seeded = true
		return paths.AtomicWrite(file, data, 0o644)
	})
	return seeded && err == nil, err
}

// ResetSettings is the owner-run `configure --restore`: copy file to file.bak.<timestamp>, then write Render whole.
func ResetSettings(file string) (backup string, err error) {
	err = paths.WithFileLock(file, func() error {
		mode := os.FileMode(0o644)
		old, rerr := os.ReadFile(file)
		switch {
		case rerr == nil:
			if fi, serr := os.Stat(file); serr == nil {
				mode = fi.Mode().Perm()
			}
			backup = file + ".bak." + time.Now().Format("20060102-150405.000000000")
			if err := writeBackup(backup, old, mode); err != nil {
				return fmt.Errorf("backup %s: %w", file, err)
			}
		case !errors.Is(rerr, fs.ErrNotExist):
			return fmt.Errorf("read %s: %w", file, rerr)
		}
		data, err := Render()
		if err != nil {
			return err
		}
		return paths.AtomicWrite(file, data, mode)
	})
	return backup, err
}

// writeBackup creates the backup exclusively so a same-instant restore never truncates an earlier one.
func writeBackup(name string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
