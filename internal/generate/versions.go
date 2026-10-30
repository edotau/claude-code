// Vendor-file versions, baselines and lost-line detection behind commitFile, plus `configure --revert`.
package generate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings/codexsettings"
)

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
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	return matches
}

// newestMirror returns path's newest cache-mirrored backup, if any.
func newestMirror(path string) (string, bool) {
	m := mirrorsOf(path)
	if len(m) == 0 {
		return "", false
	}
	return m[0], true
}

// pruneMirror keeps only the newest 5 mirrored backups of path.
func pruneMirror(path string) {
	const keep = 5
	versions := mirrorsOf(path)
	for _, v := range versions[min(keep, len(versions)):] {
		_ = os.Remove(v) // best-effort prune; a leftover old version is harmless
	}
}

// vendorBaselinePath is where the bytes this tool last wrote to path are cached, keyed by label+path.
func vendorBaselinePath(label, path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(paths.StateDir(), "vendor-baseline", label+"-"+hex.EncodeToString(sum[:4]))
}

// writeVendorBaseline caches next as the baseline for path's next commitFile call.
func writeVendorBaseline(label, path string, next []byte) error {
	if label == "codex" {
		return codexsettings.SaveBaseline(path, next)
	}
	return paths.AtomicWrite(vendorBaselinePath(label, path), next, 0o600)
}

// lostLinesFor reports theirs' (the current on-disk file) lines that are absent from both the saved
// baseline and the about-to-be-written next: edits the user made since the last render that this write
// would erase. No baseline yet means no basis for comparison (today's seed-and-write behavior).
func lostLinesFor(label, path string, theirs, next []byte) ([]string, error) {
	var base []byte
	var err error
	if label == "codex" {
		base, err = codexsettings.LoadBaseline(path)
	} else {
		base, err = os.ReadFile(vendorBaselinePath(label, path))
		if err != nil {
			return nil, nil
		}
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
	seen := map[string]bool{}
	for l := range in(theirs) {
		if !baseSet[l] && !nextSet[l] && !seen[l] {
			seen[l] = true
			lost = append(lost, l)
		}
	}
	sort.Strings(lost) // deterministic order for output and tests
	return lost
}

// printLostLines indents each lost line for display under a warning/prompt header.
func printLostLines(out io.Writer, lines []string) {
	for _, l := range lines {
		fmt.Fprintf(out, "    %s\n", l)
	}
}

// --- revert: step each vendor file back to its newest backup version ---

// labelPath is one file this tool can write, named by its commitFile label.
type labelPath struct{ label, path string }

// vendorLabelPaths lists the files commitFile writes for one top-level vendor name.
func vendorLabelPaths(vendor, home string) ([]labelPath, error) {
	if vendor == "codex" {
		path, err := codexsettings.ConfigPath(home)
		if err != nil {
			return nil, err
		}
		return []labelPath{{"codex", path}}, nil
	}
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	switch vendor {
	case "gemini":
		return []labelPath{{"gemini-settings", filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")}}, nil
	case "copilot":
		return []labelPath{
			{"copilot-hooks", filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")},
			{"copilot-settings", filepath.Join(home, ".copilot", "settings.json")},
		}, nil
	}
	return nil, nil
}

// Revert restores each selected vendor's files to their newest backup version, removing the restored version so a second revert steps one version further back.
func Revert(o Options) error {
	agents := o.Agents
	if len(agents) == 0 {
		agents = DefaultAgents
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	for _, a := range agents {
		files, err := vendorLabelPaths(a, o.Home)
		if err != nil {
			return fmt.Errorf("generate: %w", err)
		}
		for _, lp := range files {
			if err := revertOne(lp.label, lp.path, o.DryRun, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// legacyBackupSource picks the newest pre-mirror "<path>.backup.<stamp>" file, else the plain ".backup"; codex restore only.
// Migration: remove after 2026-11-13 once every machine has run a post-2026-10-28 build.
func legacyBackupSource(path string) (src string, stepBack, ok bool) {
	if versions, err := filepath.Glob(path + ".backup.*"); err == nil && len(versions) > 0 {
		sort.Strings(versions)
		return versions[len(versions)-1], true, true
	}
	src = path + ".backup"
	_, err := os.Stat(src)
	return src, false, err == nil
}

// revertOne restores path from its newest cache-mirrored backup and removes it so the next revert goes further back.
func revertOne(label, path string, dryRun bool, out io.Writer) error {
	if label == "codex" {
		return restoreCodexConfig(path, dryRun, out)
	}
	src, ok := newestMirror(path)
	consumed := true
	if !ok {
		var legacyOK bool
		src, _, legacyOK = legacyBackupSource(path)
		if !legacyOK {
			fmt.Fprintf(out, "%s: no backup to revert for %s\n", label, path)
			return nil
		}
		consumed = false
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
