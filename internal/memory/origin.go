package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

// OriginFile is the versioned identity record at the top of every repo store.
const OriginFile = ".origin"

// OriginVersion is the schema version ReadOrigin accepts.
const OriginVersion = 1

// Origin ties a store directory to the canonical checkout it serves; Aliases are explicit, never inferred.
type Origin struct {
	Version int      `json:"version"`
	Key     string   `json:"key"`               // the directory name under StoreDir(); HarnessKey for the config checkout
	Origin  string   `json:"origin"`            // canonical main-checkout path (EvalSymlinks); "" only for the store root
	Aliases []string `json:"aliases,omitempty"` // other canonical checkout paths accepted as this repo — explicit only
	Display []string `json:"display,omitempty"` // human names; never used for matching
}

// ErrOriginConflict is returned instead of ever overwriting a .origin that names a different repo.
var ErrOriginConflict = errors.New(".origin exists with a different canonical origin")

type originCacheEntry struct {
	info   os.FileInfo
	origin Origin
}

var originCache sync.Map

// ReadOrigin loads and validates dir/.origin; dir must be a real directory, not a symlink.
func ReadOrigin(dir string) (Origin, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return Origin{}, err
	}
	if !fi.IsDir() {
		return Origin{}, fmt.Errorf("%s: not a directory", dir)
	}
	path := filepath.Join(dir, OriginFile)
	info, err := os.Stat(path)
	if err != nil {
		return Origin{}, err
	}
	if cached, ok := originCache.Load(path); ok {
		c := cached.(originCacheEntry)
		if os.SameFile(info, c.info) && info.Size() == c.info.Size() && info.ModTime().Equal(c.info.ModTime()) {
			o := c.origin
			o.Aliases = slices.Clone(o.Aliases)
			o.Display = slices.Clone(o.Display)
			return o, nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Origin{}, err
	}
	var o Origin
	if err := json.Unmarshal(data, &o); err != nil {
		return Origin{}, fmt.Errorf("%s/%s: %w", dir, OriginFile, err)
	}
	if err := validateOrigin(dir, o); err != nil {
		return Origin{}, err
	}
	o.Aliases = cleanAliases(o.Aliases)
	originCache.Store(path, originCacheEntry{info, o})
	o.Aliases = slices.Clone(o.Aliases)
	o.Display = slices.Clone(o.Display)
	return o, nil
}

// validateOrigin rejects records that do not describe dir itself.
func validateOrigin(dir string, o Origin) error {
	switch {
	case o.Version != OriginVersion:
		return fmt.Errorf("%s/%s: version %d, want %d", dir, OriginFile, o.Version, OriginVersion)
	case o.Key != filepath.Base(dir):
		return fmt.Errorf("%s/%s: key %q does not match directory", dir, OriginFile, o.Key)
	case o.Origin == "" && !paths.SamePath(dir, StoreDir()):
		return fmt.Errorf("%s/%s: empty origin", dir, OriginFile)
	}
	return nil
}

// cleanAliases cleans, drops empties and dedupes while keeping order.
func cleanAliases(in []string) []string {
	var out []string
	for _, a := range in {
		if a == "" {
			continue
		}
		if a = filepath.Clean(a); !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// WriteOrigin creates dir/.origin; an existing record for the same Key+Origin is kept byte-for-byte,
// any other existing record (including an unreadable one) is never overwritten.
func WriteOrigin(dir string, o Origin) error {
	if err := validateOrigin(dir, o); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(dir, OriginFile)); err == nil {
		cur, err := ReadOrigin(dir)
		if err != nil {
			return fmt.Errorf("refusing to overwrite: %w", err)
		}
		if cur.Key == o.Key && cur.Origin == o.Origin {
			return nil
		}
		return fmt.Errorf("%s: %w", dir, ErrOriginConflict)
	}
	return putOrigin(dir, o)
}

// putOrigin writes the record unconditionally; callers have already proven the overwrite is legal.
func putOrigin(dir string, o Origin) error {
	o.Aliases = cleanAliases(o.Aliases)
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	originCache.Delete(filepath.Join(dir, OriginFile))
	return paths.AtomicWrite(filepath.Join(dir, OriginFile), append(data, '\n'), 0o644)
}
