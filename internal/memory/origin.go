package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// legacyOriginFile is the pre-vault per-store record; resolveOrigins imports it into the vault once and removes it.
const legacyOriginFile = ".origin"

// OriginVersion is the schema version ReadOrigin accepts.
const OriginVersion = 1

// Origin ties a store directory to the canonical checkout it serves; Aliases are explicit, never inferred.
type Origin struct {
	Version int      `json:"version"`
	Key     string   `json:"key"`               // the directory name under StoreDir(); harnessStoreKey() for the config checkout
	Origin  string   `json:"origin"`            // canonical main-checkout path (EvalSymlinks); "" only for the store root
	Aliases []string `json:"aliases,omitempty"` // other canonical checkout paths accepted as this repo — explicit only
	Display []string `json:"display,omitempty"` // human names; never used for matching
}

// ErrOriginConflict is returned instead of ever overwriting an origin record that names a different repo.
var ErrOriginConflict = errors.New("origin record exists with a different canonical origin")

// ReadOrigin loads and validates dir's origin record; dir must be a real directory directly under StoreDir().
func ReadOrigin(dir string) (Origin, error) {
	key, err := originKey(dir)
	if err != nil {
		return Origin{}, err
	}
	found, errs := resolveOrigins([]string{key})
	if o, ok := found[key]; ok {
		return o, nil
	}
	return Origin{}, errs[key]
}

// originKey is dir's vault row key (its store key); symlinks and dirs outside the store have none.
func originKey(dir string) (string, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s: not a directory", dir)
	}
	if !paths.SamePath(filepath.Dir(dir), StoreDir()) {
		return "", fmt.Errorf("%s: not a store directory under %s", dir, StoreDir())
	}
	return filepath.Base(dir), nil
}

// vaultRetryAt defers vault reads after one failed (each waits out View's lock timeout), so a hook pays it once.
var vaultRetryAt atomic.Int64

const vaultRetryDelay = 30 * time.Second

var errVaultSkipped = errors.New("vault read skipped after a recent failure")

// originRows reads every origin row in one View, skipped while a recent read failed.
func originRows() (map[string]string, error) {
	if time.Now().UnixNano() < vaultRetryAt.Load() {
		return nil, errVaultSkipped
	}
	var rows map[string]string
	if err := vault.View(func(t *vault.Tx) error {
		rows = t.All(vault.MemoryOrigins)
		return nil
	}); err != nil {
		vaultRetryAt.Store(time.Now().Add(vaultRetryDelay).UnixNano())
		return nil, err
	}
	return rows, nil
}

// resolveOrigins maps store keys to records: vault row, else index.md. Fallback hits become
// rows unless the vault is unreadable, which leaves the call read-only. errs holds the misses.
func resolveOrigins(keys []string) (found map[string]Origin, errs map[string]error) {
	rows, verr := originRows()
	found, errs = map[string]Origin{}, map[string]error{}
	var catalog map[string]Origin
	var imports []Origin
	for _, key := range keys {
		dir := filepath.Join(StoreDir(), key)
		if raw, ok := rows[key]; ok {
			if o, err := decodeOrigin(dir, []byte(raw)); err != nil {
				errs[key] = err
			} else {
				found[key] = o
			}
			continue
		}
		if catalog == nil {
			catalog = catalogOrigins()
		}
		o, ok := catalog[key]
		if !ok {
			errs[key] = fmt.Errorf("%s: no origin record: %w", dir, fs.ErrNotExist)
			continue
		}
		if err := validateOrigin(dir, o); err != nil {
			errs[key] = err
			continue
		}
		found[key] = o
		imports = append(imports, o)
	}
	if verr == nil && len(imports) > 0 {
		_ = importOrigins(imports) // best effort on a read path: a failed import is retried by the next read
	}
	return found, errs
}

// importOrigins writes the rows still absent in one transaction, .
func importOrigins(origins []Origin) error {
	return vault.Update(func(t *vault.Tx) error {
		for _, o := range origins {
			if _, ok := t.Get(vault.MemoryOrigins, o.Key); ok {
				continue // another process recorded it first
			}
			data, err := json.Marshal(o)
			if err != nil {
				return err
			}
			if err := t.Put(vault.MemoryOrigins, o.Key, string(data)); err != nil {
				return err
			}
		}
		return nil
	})
}

// decodeOrigin parses and validates one stored record for dir.
func decodeOrigin(dir string, data []byte) (Origin, error) {
	var o Origin
	if err := json.Unmarshal(data, &o); err != nil {
		return Origin{}, fmt.Errorf("%s: origin record: %w", dir, err)
	}
	if err := validateOrigin(dir, o); err != nil {
		return Origin{}, err
	}
	o.Aliases = cleanAliases(o.Aliases)
	return o, nil
}

// validateOrigin rejects records that do not describe dir itself.
func validateOrigin(dir string, o Origin) error {
	switch {
	case o.Version != OriginVersion:
		return fmt.Errorf("%s: origin record version %d, want %d", dir, o.Version, OriginVersion)
	case o.Key != filepath.Base(dir):
		return fmt.Errorf("%s: origin record key %q does not match directory", dir, o.Key)
	case o.Origin == "" && !paths.SamePath(dir, StoreDir()):
		return fmt.Errorf("%s: origin record has an empty origin", dir)
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

// WriteOrigin records dir's origin; an existing record for the same Key+Origin is kept as is,
// any other existing record (including an unreadable one) is never overwritten.
func WriteOrigin(dir string, o Origin) error {
	if err := validateOrigin(dir, o); err != nil {
		return err
	}
	cur, err := ReadOrigin(dir)
	switch {
	case err == nil && cur.Key == o.Key && cur.Origin == o.Origin:
		return nil
	case err == nil:
		return fmt.Errorf("%s: %w", dir, ErrOriginConflict)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("refusing to overwrite: %w", err)
	}
	return foldOrigin(dir, o, "")
}

// foldOrigin writes dir's record and, when drop is set, deletes that folded store's row in the same transaction.
func foldOrigin(dir string, o Origin, drop string) error {
	key, err := originKey(dir)
	if err != nil {
		return err
	}
	o.Aliases = cleanAliases(o.Aliases)
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return vault.Update(func(t *vault.Tx) error {
		if drop != "" {
			if err := t.Put(vault.MemoryOrigins, drop, ""); err != nil {
				return err
			}
		}
		return t.Put(vault.MemoryOrigins, key, string(data))
	})
}

// catalogOriginTag opens the machine-readable origin comment BuildCatalog appends to each repo row.
const catalogOriginTag = " <!-- origin: "

// catalogOrigin is the comment's JSON payload; json.Marshal escapes '>' so a path can never close the comment.
type catalogOrigin struct {
	Origin  string   `json:"origin"`
	Aliases []string `json:"aliases,omitempty"`
}

// catalogOriginComment is the trailing comment recording o on its catalog row.
func catalogOriginComment(o Origin) string {
	data, _ := json.Marshal(catalogOrigin{Origin: o.Origin, Aliases: o.Aliases})
	return catalogOriginTag + string(data) + " -->"
}

// catalogOrigins parses the origin comments of index.md's repo rows ("- <key> — …"), keyed by store key.
func catalogOrigins() map[string]Origin {
	out := map[string]Origin{}
	data, err := os.ReadFile(filepath.Join(StoreDir(), CatalogFile))
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, rest, ok := strings.Cut(strings.TrimPrefix(line, "- "), " — ")
		_, after, ok0 := strings.CutLast(rest, catalogOriginTag)
		if !ok || !strings.HasPrefix(line, "- ") || !ok0 || !strings.HasSuffix(rest, " -->") {
			continue
		}
		var c catalogOrigin
		if json.Unmarshal([]byte(strings.TrimSuffix(after, " -->")), &c) == nil {
			out[key] = Origin{Version: OriginVersion, Key: key, Origin: c.Origin, Aliases: cleanAliases(c.Aliases)}
		}
	}
	return out
}
