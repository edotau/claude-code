package memory

// Explicit legacy→store migration (plan §4): a dry run writes a manifest, apply consumes that exact
// manifest. Nothing here runs at SessionStart; sources come only from the legacy trees, never StoreDir().

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// ManifestSchema bumps when MigrateItem/Manifest change shape; LoadManifest refuses other schemas.
const ManifestSchema = 1

// migrateStamp names manifests and receipts by UTC second.
const migrateStamp = "20060102-150405"

// Item actions.
const (
	ActionCopy       = "copy"
	ActionDelete     = "delete"
	ActionQuarantine = "quarantine"
	ActionSkip       = "skip"
)

// Item kinds (what the source is, not what happens to it).
const (
	KindGlobalBank    = "global-bank"
	KindRepoBank      = "repo-bank"
	KindRepoArchive   = "repo-archive"
	KindArchiveNative = "archive-native"
	KindNativeTopic   = "native-topic"
	KindNativeIndexIt = "native-index"
	KindNativeArchive = "native-archive"
	KindOriginItem    = "origin"
	KindHandoff       = "handoff"
	KindAccessLog     = "accesslog"
	KindLockFile      = "lockfile"
	KindLegacyIndex   = "legacy-index"
	KindPristine      = "pristine"
	KindArtifact      = "artifact"
	KindEmptyDir      = "empty-dir"
	KindPlan          = "plan"        // docs/plans file → <cfg>/plans (outside the store, not committed)
	KindReport        = "report"      // docs/reports file → <store>/reports
	KindIndexMerge    = "index-merge" // legacy native MEMORY.md merged into the store's MEMORY.md
	KindSymlink       = "symlink"     // dangling relative link; SHA256 is of its target string
	KindUnknownItem   = "unknown"
)

// MigrateItem is one planned step; Deps name items listed (and applied) earlier, e.g. a repo dir's origin.
type MigrateItem struct {
	ID     string   `json:"id"`               // hex(sha256(source rel to ConfigDir))[:12]; inline: sha256("inline:"+dest rel)
	Kind   string   `json:"kind"`             // one of the Kind* constants
	Source string   `json:"source,omitempty"` // absolute; "" for inline items
	Dest   string   `json:"dest,omitempty"`   // absolute; "" for delete/quarantine/skip
	Inline []byte   `json:"inline,omitempty"` // generated origin record for the store dir Dest (a vault row, not a file)
	Bytes  int64    `json:"bytes"`
	SHA256 string   `json:"sha256"` // of Source (or Inline)
	Action string   `json:"action"` // copy | delete | quarantine | skip
	Reason string   `json:"reason"`
	Deps   []string `json:"deps,omitempty"`
}

// Manifest is the dry-run inventory apply consumes; Origins maps every slug seen to the canonical origin used.
type Manifest struct {
	Schema    int               `json:"schema"`
	Created   time.Time         `json:"created"`
	ConfigDir string            `json:"configDir"`
	Store     string            `json:"store"`
	Origins   map[string]string `json:"origins"`
	Items     []MigrateItem     `json:"items"`
	Findings  []string          `json:"findings"` // human notices: unresolved slugs, conflicts, unknown content, quarantines
}

// ErrManifestStale rejects a manifest written for another box or schema.
var ErrManifestStale = errors.New("memory migrate: manifest schema or config dir does not match this box")

// WriteManifest stores m under MigrateStateDir() as manifest-<created UTC>.json; an existing name is never overwritten.
func WriteManifest(m Manifest) (string, error) {
	if err := os.MkdirAll(MigrateStateDir(), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	stem := filepath.Join(MigrateStateDir(), "manifest-"+m.Created.UTC().Format(migrateStamp))
	path, f, err := createUnique(stem, ".json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return "", errors.Join(err, f.Close())
	}
	return path, f.Close()
}

// createUnique O_EXCL-creates stem+ext, trying stem-2, stem-3 … while the name exists.
func createUnique(stem, ext string) (string, *os.File, error) {
	for n := 1; ; n++ {
		path := stem + ext
		if n > 1 {
			path = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if !os.IsExist(err) {
			return path, f, err
		}
	}
}

// LoadManifest reads path and proves it was made for this schema and config dir, with every Dep listed earlier.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	if m.Schema != ManifestSchema || !paths.SamePath(m.ConfigDir, paths.ConfigDir()) {
		return Manifest{}, fmt.Errorf("%s: %w", path, ErrManifestStale)
	}
	seen := map[string]bool{}
	for _, it := range m.Items {
		for _, d := range it.Deps {
			if !seen[d] {
				return Manifest{}, fmt.Errorf("%s: item %s depends on %s, which is not listed before it", path, it.ID, d)
			}
		}
		seen[it.ID] = true
	}
	return m, nil
}

// sha256File hashes a regular file; a symlink or directory is an error, never followed.
func sha256File(path string) (string, int64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s: not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// itemID is the stable short id of a source rel (or "inline:<dest rel>") under ConfigDir.
func itemID(rel string) string { return sha256Hex([]byte(rel))[:12] }

// configRel is p relative to ConfigDir, slash-separated; an outside path keeps its absolute form.
func configRel(p string) string {
	rel, err := filepath.Rel(paths.ConfigDir(), p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// isLockFile matches the harness's own beacons beside bank files: stale once the writers are quiesced.
func isLockFile(name string) bool {
	return strings.HasPrefix(name, ".") && (strings.HasSuffix(name, ".lock") ||
		strings.HasPrefix(name, ".postharvest") || strings.HasPrefix(name, ".migrate"))
}

func hasTypedFile(dir string) bool {
	for _, name := range bankFiles {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// olderTemplates are exact earlier texts of a template; a legacy bank holding only these is still untouched.
var olderTemplates = map[string][]string{
	"conventions.md": {"# Conventions\n\n> Learned patterns/preferences. APPEND; `/reflection:consolidate` prunes. Promote durable,\n" +
		"> cross-project lessons up to the shared `docs/` corpus via `/memory:promote`.\n"},
}

// pristineDir: typed files equal a template (or, when legacy, a known older one); legacy also tolerates access.log.
func pristineDir(dir string, legacy bool) bool {
	ents, _ := os.ReadDir(dir)
	typed := 0
	for _, e := range ents {
		name := e.Name()
		if e.Type().IsRegular() && (isLockFile(name) || (legacy && name == "access.log")) {
			continue
		}
		body, ok := templates()[name]
		if !ok || !e.Type().IsRegular() {
			return false
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || (string(data) != body && (!legacy || !slices.Contains(olderTemplates[name], string(data)))) {
			return false
		}
		typed++
	}
	return typed > 0
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Journal phases, in order; journalComplete (guard.go) is the last.
const (
	phaseCopy   = "copy"
	phaseCommit = "commit"
	phaseRetire = "retire"
)

// Step states recorded per item.
const (
	stepCopied      = "copied"
	stepVerified    = "verified"
	stepRetired     = "retired"
	stepDeleted     = "deleted"
	stepQuarantined = "quarantined"
	stepConflict    = "conflict"
	stepChanged     = "changed"
	stepSkipped     = "skipped"
)

// migrateJournal is the durable progress record beside the manifest; StartupGuard reads phase and manifest.
type migrateJournal struct {
	Manifest       string                 `json:"manifest"`
	ManifestSHA256 string                 `json:"manifestSha256"`
	Started        time.Time              `json:"started"`
	Phase          string                 `json:"phase"`
	Commit         string                 `json:"commit,omitempty"`
	Steps          map[string]journalStep `json:"steps"`
}

type journalStep struct {
	State      string    `json:"state"`
	DestSHA256 string    `json:"destSha256,omitempty"`
	At         time.Time `json:"at"`
}

// loadOrStartJournal resumes jpath when it belongs to this exact manifest content, else starts a fresh one.
func loadOrStartJournal(jpath, manifest, msum string) (*migrateJournal, error) {
	data, err := os.ReadFile(jpath)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(MigrateStateDir(), 0o700); err != nil {
			return nil, err
		}
		return &migrateJournal{Manifest: manifest, ManifestSHA256: msum, Started: time.Now(), Phase: phaseCopy, Steps: map[string]journalStep{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var j migrateJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("%s: %w", jpath, err)
	}
	if j.ManifestSHA256 != msum {
		return nil, fmt.Errorf("%s: journal was written for different manifest bytes", jpath)
	}
	if j.Steps == nil {
		j.Steps = map[string]journalStep{}
	}
	return &j, nil
}

// otherJournalIncomplete refuses to start while a journal for a DIFFERENT manifest is unfinished.
func otherJournalIncomplete(jpath string) error {
	matches, _ := filepath.Glob(filepath.Join(MigrateStateDir(), "*.journal.json"))
	for _, p := range matches {
		if p == jpath {
			continue
		}
		var j struct {
			Phase string `json:"phase"`
		}
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &j) == nil && j.Phase == journalComplete {
			continue
		}
		return fmt.Errorf("memory migrate: incomplete journal %s belongs to another manifest; finish or remove it first", filepath.Base(p))
	}
	return nil
}

// liveClaims: watcher claims plus every unreadable or pending harvest result; age never clears one (plan §4).
func liveClaims() []string {
	ents, _ := os.ReadDir(StateDir())
	var names []string
	for _, e := range ents {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "watch-") && !strings.HasSuffix(name, ".log"):
			names = append(names, name)
		case strings.HasPrefix(name, "harvest-result-"):
			r, ok := ReadHarvestResult(strings.TrimPrefix(name, "harvest-result-"))
			switch {
			case !ok:
				names = append(names, name+" (unreadable)")
			case r.Status == HarvestPending:
				names = append(names, name)
			}
		}
	}
	return names
}

// copyExclusive is the no-clobber primitive: O_EXCL create, write, fsync, re-hash; EEXIST compares instead.
func copyExclusive(src, dest, want string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		if h, _, err := sha256File(dest); err == nil && h == want {
			return stepVerified, nil
		}
		return stepConflict, nil
	}
	if err != nil {
		return "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", errors.Join(err, f.Close(), os.Remove(dest))
	}
	defer in.Close()
	if _, err = io.Copy(f, in); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if h, _, herr := sha256File(dest); herr != nil || h != want {
			err = fmt.Errorf("%s: copy verification failed", dest)
		}
	}
	if err != nil {
		return "", errors.Join(err, os.Remove(dest))
	}
	return stepCopied, nil
}

// mergeOriginAliases is AddOriginAlias without its bank lock (apply already holds it): same Key+Origin only.
func mergeOriginAliases(dir string, o Origin) error {
	cur, err := ReadOrigin(dir)
	if err != nil || cur.Key != o.Key || !paths.SamePath(cur.Origin, o.Origin) {
		return fmt.Errorf("%s: %w", dir, ErrOriginConflict)
	}
	entries, err := RepoDirs()
	if err != nil {
		return err
	}
	added := false
	for _, alias := range o.Aliases {
		if findEntry([]RepoEntry{{Dir: dir, Origin: cur}}, alias) != nil {
			continue
		}
		if e := findEntry(entries, alias); e != nil && !paths.SamePath(e.Dir, dir) {
			return fmt.Errorf("%s already belongs to %s: %w", alias, e.Dir, ErrOriginConflict)
		}
		cur.Aliases = append(cur.Aliases, alias)
		added = true
	}
	if !added {
		return nil
	}
	return putOrigin(dir, cur)
}
