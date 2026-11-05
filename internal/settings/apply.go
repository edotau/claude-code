package settings

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/paths"
)

// SettingsFile is the live Claude Code settings.json under the config dir.
func SettingsFile() string { return filepath.Join(paths.ConfigDir(), "settings.json") }

// ApplySettings is the read/render/merge/write of settings.json shared by install and configure's exit re-render:
// a missing file is generated from the defaults; an existing one has only its hooks refreshed.
func ApplySettings(dryRun bool, notice io.Writer) (file string, existing, merged []byte, sum Summary, backup string, err error) {
	file = SettingsFile()
	existing, err = os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return file, nil, nil, Summary{}, "", err
	}
	if restoreMissingSettings(err, dryRun) { // a fresh install restores the defaults whole
		merged, sum, backup, err = restoreNewSettings(file, notice)
		return file, nil, merged, sum, backup, err
	}
	rendered, err := RenderLive()
	if err != nil {
		return file, existing, nil, Summary{}, "", err
	}
	merged, sum, err = Merge(existing, rendered)
	if err != nil {
		return file, existing, nil, Summary{}, "", err
	}
	if string(merged) == string(existing) || dryRun {
		return file, existing, merged, sum, "", nil
	}
	merged, sum, backup, err = updateMergedSettings(file, rendered, merged, sum)
	return file, existing, merged, sum, backup, err
}

func restoreNewSettings(file string, notice io.Writer) ([]byte, Summary, string, error) {
	r, err := RestoreSettings(file, notice)
	if err != nil {
		return nil, Summary{}, "", err
	}
	_, sum, err := Merge(nil, r.Data)
	return r.Data, sum, r.Backup, err
}

func updateMergedSettings(file string, rendered, merged []byte, sum Summary) ([]byte, Summary, string, error) {
	backup, err := UpdateWithBackup(file, func(old []byte) ([]byte, error) {
		var mergeErr error // re-merge the bytes read under the lock, so a concurrent writer's update survives
		merged, sum, mergeErr = Merge(old, rendered)
		return merged, mergeErr
	})
	return merged, sum, backup, err
}

func restoreMissingSettings(err error, dryRun bool) bool {
	return errors.Is(err, fs.ErrNotExist) && !dryRun
}

// RestoreResult is what RestoreSettings did: the rendered bytes, the history commit and the backup path.
type RestoreResult struct {
	Data   []byte
	Commit string
	Backup string
}

// RestoreSettings snapshots history ("before restore"), renders defaults plus overlay and writes file whole.
func RestoreSettings(file string, notice io.Writer) (RestoreResult, error) {
	var r RestoreResult
	if h, err := history.SnapshotConfig("before restore"); err != nil {
		fmt.Fprintln(notice, "restore: history snapshot:", err)
	} else {
		r.Commit = h.Commit
	}
	data, err := RenderLive()
	if err != nil {
		return r, err
	}
	r.Data = data
	r.Backup, err = WriteWithBackup(file, data)
	return r, err
}
