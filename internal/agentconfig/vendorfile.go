// Agent-file versions, baselines and lost-line detection behind CommitFile, plus the per-file revert.
package agentconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// RenderOptions controls one generate pass.
type RenderOptions struct {
	Agents     []string  // agents to render; default DefaultAgents
	DryRun     bool      // print the rendered blocks; write nothing
	Check      bool      // report drift; never writes
	Home       string    // overrides os.UserHomeDir() for agent config paths (tests)
	Out        io.Writer // status lines; nil -> os.Stdout
	ForceHomes bool      // force agent-config renders even when CLAUDE_CONFIG_DIR is not the default ~/.claude
	// Confirm asks whether to overwrite path given its lost hand-edited lines; nil means write and warn.
	Confirm func(path string, lost []string) bool
	// staged is the write queue CommitFile appends to; Stage sets it, RenderPlan.Apply runs it.
	staged *[]stagedWrite
}

// RenderPlan holds ordered agent writes after all selected renders have validated.
type RenderPlan struct {
	writes []stagedWrite
}

// stagedWrite is one queued write; path names the file Apply snapshots for rollback ("" for none, like a baseline seed).
type stagedWrite struct {
	path  string
	write func() error
	undo  func() error // optional: reverses write for state outside path (a vault baseline); nil for none
}

// Apply performs the staged writes in order; on the first error it restores every config file already written.
func (p *RenderPlan) Apply() error {
	var done []snapshot
	for _, w := range p.writes {
		snap, err := takeSnapshot(w.path)
		if err != nil {
			return errors.Join(err, restore(done))
		}
		if err := w.write(); err != nil {
			return errors.Join(err, restore(append(done, snap)))
		}
		snap.undo = w.undo
		done = append(done, snap)
	}
	return nil
}

// snapshot is a config file's bytes before a staged write; absent means it did not exist.
type snapshot struct {
	path   string
	data   []byte
	absent bool
	undo   func() error
}

func takeSnapshot(path string) (snapshot, error) {
	if path == "" {
		return snapshot{}, nil
	}
	data, existed, err := ReadExisting(path)
	return snapshot{path: path, data: data, absent: !existed && err == nil}, err
}

// restore puts snapshots back newest first, so a failed multi-file render leaves no half-applied pair.
func restore(done []snapshot) error {
	var errs []error
	for _, s := range slices.Backward(done) {
		if s.undo != nil {
			errs = append(errs, s.undo())
		}
		switch {
		case s.path == "":
		case s.absent:
			if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		default:
			errs = append(errs, paths.AtomicWrite(s.path, s.data, 0o600))
		}
	}
	return errors.Join(errs...)
}

// queue appends w to the staged writes, or runs it at once when the options were not built with Stage.
func (o RenderOptions) queue(w stagedWrite) error {
	if o.staged == nil {
		return w.write()
	}
	*o.staged = append(*o.staged, w)
	return nil
}

// Stage returns o with CommitFile writes queued onto p instead of written.
func (o RenderOptions) Stage(p *RenderPlan) RenderOptions {
	o.staged = &p.writes
	return o
}

// CommitFile is the shared dry-run/check/write decision once a renderer has the next byte slice.
func CommitFile(path string, raw, next []byte, existed bool, o RenderOptions, out io.Writer, label string) (bool, error) {
	changed, done, err := previewOrCheck(path, raw, next, o, out, label, func(changed bool) error {
		return reportCheck(path, raw, next, changed, out, label)
	})
	switch {
	case done:
		return changed, err
	case !changed:
		return false, reportUnchanged(path, next, existed, o, out, label)
	}
	// The no-op write snapshots the default baseline file; a fileOps baseline (codex vault) is restored by its undo.
	pre := stagedWrite{path: baselinePath(label, path), write: func() error { return nil }}
	if ops := fileOps[label]; ops.LoadBaseline != nil && ops.SaveBaseline != nil {
		var prev []byte
		pre.write = func() (err error) { prev, err = ops.LoadBaseline(path); return err }
		pre.undo = func() error {
			if prev == nil { // no previous baseline to put back: the ops cannot delete one
				return nil
			}
			return ops.SaveBaseline(path, prev)
		}
	}
	if err := o.queue(pre); err != nil {
		return true, err
	}
	return true, o.queue(stagedWrite{path: path, write: func() error { return commitWrite(path, raw, next, existed, o, out, label) }})
}

// previewOrCheck is the dry-run/check half of every commit: done means the mode handled it, check runs for --check.
func previewOrCheck(path string, raw, next []byte, o RenderOptions, out io.Writer, label string, check func(changed bool) error) (changed, done bool, err error) {
	changed = !bytes.Equal(next, raw)
	switch {
	case o.DryRun:
		fmt.Fprintf(out, "--- %s (%s) ---\n%s\n", path, label, next)
		return changed, true, nil
	case o.Check:
		return changed, true, check(changed)
	}
	return changed, false, nil
}

// reportCheck prints --check drift and any hand-edited lines the render would lose.
func reportCheck(path string, raw, next []byte, changed bool, out io.Writer, label string) error {
	if changed {
		fmt.Fprintf(out, "%s: drift at %s\n", label, path)
	}
	lost, err := lostLinesFor(label, path, raw, next)
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) not in this render:\n", label, path, len(lost))
		printLostLines(out, lost)
	}
	return nil
}

// reportUnchanged prints the "unchanged" line, first seeding or staging the baseline for an unchanged file.
func reportUnchanged(path string, next []byte, existed bool, o RenderOptions, out io.Writer, label string) error {
	if ops := fileOps[label]; ops.LoadBaseline != nil {
		base, err := ops.LoadBaseline(path)
		if err != nil {
			return err
		}
		if base == nil && existed {
			if err := o.queue(stagedWrite{write: func() error { return ops.SaveBaseline(path, next) }}); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return nil
	}
	// Seed a missing baseline from an unchanged file, or later hand edits go undetected.
	if _, err := os.Stat(baselinePath(label, path)); os.IsNotExist(err) && existed {
		if err := o.queue(stagedWrite{write: func() error { return seedBaseline(label, path, next, out) }}); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
	return nil
}

// seedBaseline writes next as path's baseline under the lock unless a concurrent write created one; best-effort.
func seedBaseline(label, path string, next []byte, out io.Writer) error {
	err := paths.WithFileLock(baselinePath(label, path), func() error {
		if _, err := os.Stat(baselinePath(label, path)); !os.IsNotExist(err) {
			return nil
		}
		return writeBaseline(label, path, next)
	})
	if err != nil { // a lock or write failure must not fail Apply and roll back the config writes before it
		fmt.Fprintf(out, "%s: baseline: %v (next run can't detect hand edits)\n", label, err)
	}
	return nil
}

// commitWrite runs the lost-lines check (prompting or warning), writes path, and refreshes its
// baseline; it is the body of every CommitFile staged write.
func commitWrite(path string, raw, next []byte, existed bool, o RenderOptions, out io.Writer, label string) error {
	lost, err := lostLinesFor(label, path, raw, next)
	if err != nil {
		return err
	}
	if len(lost) > 0 && !confirmLost(path, lost, o, out, label) {
		return nil
	}
	return paths.WithFileLock(baselinePath(label, path), func() error {
		return writeLocked(path, raw, next, existed, out, label)
	})
}

// confirmLost reports lines the write would lose and returns false when the user kept their edits.
func confirmLost(path string, lost []string, o RenderOptions, out io.Writer, label string) bool {
	if o.Confirm == nil {
		fmt.Fprintf(out, "%s: overwrote %d hand-edited line(s) in %s; `claude-code configure --revert` restores\n",
			label, len(lost), path)
		printLostLines(out, lost)
		return true
	}
	fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) that would be lost:\n", label, path, len(lost))
	printLostLines(out, lost)
	if !o.Confirm(path, lost) {
		fmt.Fprintf(out, "%s: kept %s (your edits)\n", label, path)
		return false
	}
	return true
}

// writeLocked re-checks path is unchanged since read, writes it, and refreshes its baseline.
func writeLocked(path string, raw, next []byte, existed bool, out io.Writer, label string) error {
	if cur, err := os.ReadFile(path); (err == nil) != existed || (existed && !bytes.Equal(cur, raw)) {
		return fmt.Errorf("%s: %s changed since it was read; re-run", label, path)
	}
	write := backupAndWrite
	if ops := fileOps[label]; ops.Write != nil {
		write = ops.Write
	}
	if err := write(path, raw, next, existed); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := writeBaseline(label, path, next); err != nil {
		fmt.Fprintf(out, "%s: baseline: %v (next run can't detect hand edits)\n", label, err)
	}
	fmt.Fprintf(out, "%s: wrote %s\n", label, path)
	return nil
}

// CommitOverlayFile is CommitFile's dry-run/check/write decision, minus the lost-hand-edited-lines
// check and baseline tracking: the overlay is never hand-edited, so there is nothing to lose.
func CommitOverlayFile(path string, raw, next []byte, o RenderOptions, out io.Writer, label string) (bool, error) {
	changed, done, err := previewOrCheck(path, raw, next, o, out, label, func(changed bool) error {
		if changed {
			fmt.Fprintf(out, "%s: drift at %s\n", label, path)
		}
		return nil
	})
	switch {
	case done:
		return changed, err
	case !changed:
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	return true, o.queue(stagedWrite{path: path, write: func() error {
		if _, err := paths.WriteIfChanged(path, next, 0o600); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		fmt.Fprintf(out, "%s: wrote %s\n", label, path)
		return nil
	}})
}

var _ = schemas.Register(schemas.Format{
	Name: "vendor-baseline", Owner: "internal/agentconfig", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "vendor-baseline", "*") },
	Version: 1, Keep: "raw bytes last written to a vendor file; reseeded when missing",
})

// backupAndWrite saves oldRaw (only when the file already existed) to the cache mirror, keeps the newest 5
// mirror versions, then atomically replaces path with newRaw.
func backupAndWrite(path string, oldRaw, newRaw []byte, existed bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if existed {
		if _, err := paths.Backup(path, oldRaw); err != nil {
			return err
		}
		pruneMirror(path)
	}
	return paths.AtomicWrite(path, newRaw, 0o600)
}

// mirrorsOf returns path's cache-mirrored backups, newest first (the timestamp format sorts lexically).
func mirrorsOf(path string) []string {
	sample := paths.BackupPath(path, time.Now())
	prefix := sample[:strings.LastIndex(sample, ".bak-")+len(".bak-")]
	matches, _ := filepath.Glob(prefix + "*")
	slices.Sort(matches)
	slices.Reverse(matches)
	return matches
}

// pruneMirror keeps only the newest 5 mirrored backups of path.
func pruneMirror(path string) {
	const keep = 5
	versions := mirrorsOf(path)
	for _, v := range versions[min(keep, len(versions)):] {
		_ = os.Remove(v) // best-effort prune; a leftover old version is harmless
	}
}

// baselinePath is where the bytes this tool last wrote to path are cached, keyed by label+path.
func baselinePath(label, path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(paths.StateDir(), "vendor-baseline", label+"-"+hex.EncodeToString(sum[:4]))
}

// writeBaseline caches next as the baseline for path's next CommitFile call.
func writeBaseline(label, path string, next []byte) error {
	if ops := fileOps[label]; ops.SaveBaseline != nil {
		return ops.SaveBaseline(path, next)
	}
	return paths.AtomicWrite(baselinePath(label, path), next, 0o600)
}

// lostLinesFor reports theirs' (the current on-disk file) lines that are absent from both the saved
// baseline and the about-to-be-written next: edits the user made since the last render that this write
// would erase. No baseline yet means no basis for comparison (today's seed-and-write behavior).
func lostLinesFor(label, path string, theirs, next []byte) ([]string, error) {
	var base []byte
	var err error
	if ops := fileOps[label]; ops.LoadBaseline != nil {
		base, err = ops.LoadBaseline(path)
	} else {
		base, err = os.ReadFile(baselinePath(label, path))
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if base == nil {
		return nil, nil
	}
	return lostLines(theirs, base, next), nil
}

// lostLines returns theirs-only lines (trimmed, non-blank, de-duplicated) that are in neither base nor next.
func lostLines(theirs, base, next []byte) []string {
	in := func(b []byte) map[string]bool {
		m := map[string]bool{}
		for l := range strings.SplitSeq(string(b), "\n") {
			if l = strings.TrimRight(l, " \t\r"); l != "" {
				m[l] = true
			}
		}
		return m
	}
	baseSet, nextSet := in(base), in(next)
	var lost []string
	for l := range in(theirs) {
		if !baseSet[l] && !nextSet[l] {
			lost = append(lost, l)
		}
	}
	sort.Strings(lost) // deterministic order for output and tests
	return lost
}

// printLostLines indents each lost line for display under a warning/prompt header, credential shapes redacted.
func printLostLines(out io.Writer, lines []string) {
	for _, l := range lines {
		fmt.Fprintf(out, "    %s\n", textutil.RedactSecrets(l))
	}
}

// --- revert: step each agent file back to its newest backup version ---

// FileOps are a vendor file's deviations from CommitFile's default flow, registered per label from an init.
type FileOps struct {
	Write        func(path string, oldRaw, newRaw []byte, existed bool) error // default backupAndWrite
	LoadBaseline func(path string) ([]byte, error)                            // default: the cached bytes
	SaveBaseline func(path string, next []byte) error
	Revert       func(path string, dryRun bool, out io.Writer) error
}

var fileOps = map[string]FileOps{}

// RegisterFileOps sets label's FileOps; it runs from an init, never at call time.
func RegisterFileOps(label string, ops FileOps) { fileOps[label] = ops }

// LabelPath is one file this tool can write, named by its CommitFile label.
type LabelPath struct{ Label, Path string }

// HomeOr is home, else the user's home directory.
func HomeOr(home string) (string, error) {
	if home != "" {
		return home, nil
	}
	return os.UserHomeDir()
}

// LegacyBackupSource picks the newest pre-mirror "<path>.backup.<stamp>" file, else the plain ".backup"; RevertOne and codex restore.
// Migration: remove after 2026-11-13 once every machine has run a post-2026-10-28 build.
func LegacyBackupSource(path string) (src string, stepBack, ok bool) {
	if versions, err := filepath.Glob(path + ".backup.*"); err == nil && len(versions) > 0 {
		sort.Strings(versions)
		return versions[len(versions)-1], true, true
	}
	src = path + ".backup"
	_, err := os.Stat(src)
	return src, false, err == nil
}

// RevertOne restores path from its newest cache-mirrored backup and removes it so the next revert goes further back.
func RevertOne(label, path string, dryRun bool, out io.Writer) error {
	if ops := fileOps[label]; ops.Revert != nil {
		return ops.Revert(path, dryRun, out)
	}
	src, ok := "", false
	if m := mirrorsOf(path); len(m) > 0 {
		src, ok = m[0], true
	}
	consumed := true
	if !ok {
		var stepBack, legacyOK bool
		src, stepBack, legacyOK = LegacyBackupSource(path)
		if !legacyOK {
			fmt.Fprintf(out, "%s: no backup to revert for %s\n", label, path)
			return nil
		}
		consumed = stepBack // a versioned sidecar is consumed so the next revert steps back; plain .backup stays
	}
	if dryRun {
		fmt.Fprintf(out, "%s: would restore %s from %s\n", label, path, src)
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := paths.AtomicWrite(path, data, 0o600); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if consumed {
		_ = os.Remove(src) // consumed: the next revert falls through to the next-newest version
	}
	fmt.Fprintf(out, "%s: restored %s from %s\n", label, path, src)
	return nil
}

// ReadExisting reads path; a missing file is (nil, false, nil), any other read failure is an error.
func ReadExisting(path string) (raw []byte, existed bool, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// CommitJSONObject reads path as a JSON object, lets mutate edit it, and commits the result. An unreadable or
// non-object file is skipped with a "label: skipped" note and left untouched; mutate returning false writes nothing.
func CommitJSONObject(path string, o RenderOptions, out io.Writer, label string, mutate func(obj map[string]any, existed bool) bool) (bool, error) {
	raw, existed, err := ReadExisting(path)
	if err != nil {
		fmt.Fprintf(out, "%s: skipped: cannot read %s: %v\n", label, path, err)
		return false, nil
	}
	obj := map[string]any{}
	before := []byte("{}") // a blank existing file is an empty object, as in DecodeRawObject
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // numbers stay verbatim, so the equality check below never loses float precision
		err := dec.Decode(&obj)
		if err == nil {
			if _, terr := dec.Token(); terr != io.EOF { // More() is false for a stray } or ]
				err = errors.New("trailing data after the top-level value")
			}
		}
		if err != nil {
			fmt.Fprintf(out, "%s: skipped: %s is not plain JSON: %v\n", label, path, err)
			return false, nil
		}
		if obj == nil { // "null" decodes without error but leaves obj nil, not an empty object
			fmt.Fprintf(out, "%s: skipped: %s is not plain JSON: not an object\n", label, path)
			return false, nil
		}
		if before, err = textutil.MarshalNoEscape(obj); err != nil {
			return false, fmt.Errorf("%s: %w", label, err)
		}
	}
	if !mutate(obj, existed) {
		return false, nil
	}
	body, err := textutil.MarshalIndentNoEscape(obj)
	if err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	next := append(body, '\n')
	// Same values as the file: its key order, spacing and \u003c escapes stay as the vendor wrote them (no rewrite).
	if after, err := textutil.MarshalNoEscape(obj); existed && err == nil && bytes.Equal(before, after) {
		next = raw
	}
	return CommitFile(path, raw, next, existed, o, out, label)
}

// RenderHooksFile writes body to path, merged over whatever is already there.
func RenderHooksFile(path string, body []byte, merge func(existing, body []byte) ([]byte, error), o RenderOptions, out io.Writer, label string) (bool, error) {
	raw, existed, err := ReadExisting(path)
	if err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	next, err := merge(raw, body)
	if err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	return CommitFile(path, raw, append(next, '\n'), existed, o, out, label)
}

// DecodeRawObject decodes raw as a JSON object, keeping every key as raw bytes; blank decodes to empty, JSON null is rejected.
func DecodeRawObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil { // json null unmarshals to a nil map: not an object, so refuse to overwrite it
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

// decodeRawEvents decodes a "hooks" field (event name -> array of raw group/entry objects); a nil or
// absent field decodes to an empty map.
func decodeRawEvents(raw json.RawMessage) (map[string][]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string][]json.RawMessage{}, nil
	}
	var m map[string][]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string][]json.RawMessage{}
	}
	return m, nil
}

// mergeHookEvents keeps every existing item of each event in its original relative position, dropping
// an item's harness-owned part per classify; the freshly rendered block is spliced in as a whole at the
// position of the FIRST existing item classify found harness-owned (prepended when none was); an event
// left with nothing is dropped. Idempotent: merging an already-merged event over the same rendered
// content reproduces it byte-for-byte, since the rendered block is itself wholly harness-owned next time
// round. Shared by the codex and copilot hook mergers.
func mergeHookEvents[T any](existing, rendered map[string][]T, classify func(T) (*T, bool)) map[string][]T {
	merged := map[string][]T{}
	for event, items := range existing {
		merged[event] = spliceEvent(items, rendered[event], classify)
	}
	for event, items := range rendered {
		if _, ok := existing[event]; !ok {
			merged[event] = items
		}
	}
	for event, items := range merged {
		if len(items) == 0 {
			delete(merged, event)
		}
	}
	return merged
}

// spliceEvent drops each item's harness-owned part per classify and splices block in at the first owned position.
func spliceEvent[T any](items, block []T, classify func(T) (*T, bool)) []T {
	var out []T
	insertAt := -1
	for _, item := range items {
		remainder, harnessOwned := classify(item)
		if harnessOwned && insertAt == -1 {
			insertAt = len(out)
		}
		if remainder != nil {
			out = append(out, *remainder)
		}
	}
	if insertAt == -1 {
		insertAt = 0 // no harness-owned item in this event: prepend, as if nothing existed before
	}
	result := make([]T, 0, len(out)+len(block))
	result = append(result, out[:insertAt]...)
	result = append(result, block...)
	result = append(result, out[insertAt:]...)
	return result
}

// MergeHooksTop merges body's "hooks" over existingRaw's per classify; next is the existing top level, updated.
func MergeHooksTop(existingRaw, body []byte, classify func(json.RawMessage) (*json.RawMessage, bool)) (next, renderedTop map[string]json.RawMessage, err error) {
	existingTop, err := DecodeRawObject(existingRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("existing file: %w", err)
	}
	renderedTop, err = DecodeRawObject(body)
	if err != nil {
		return nil, nil, err
	}
	existingHooks, err := decodeRawEvents(existingTop["hooks"])
	if err != nil {
		return nil, nil, fmt.Errorf("existing file: %w", err)
	}
	renderedHooks, err := decodeRawEvents(renderedTop["hooks"])
	if err != nil {
		return nil, nil, err
	}
	hooksRaw, err := textutil.MarshalNoEscape(mergeHookEvents(existingHooks, renderedHooks, classify))
	if err != nil {
		return nil, nil, err
	}
	next = maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
	return next, renderedTop, nil
}
