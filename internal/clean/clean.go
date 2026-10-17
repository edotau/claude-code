// Package clean is the explicit state sweep: `claude-code clean` is dry-run by default, and the
// age-out every target protects against is otherwise a LAZY side effect (harvest, hooks, install
// each prune only their own path on their own schedule — nothing ever sweeps the rest).
//
// Invariants, in order of importance:
//  1. Credentials are NEVER touched: env.d/*.env, settings.json, providers.json, .credentials.json.
//  2. The memory bank (docs/memory/bank/**, including its archive/ and archive/native/ subtrees) is
//     NEVER a target, not even opt-in — it holds promoted, irrecoverable history.
//  3. Nothing is deleted without --apply.
package clean

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// target is one sweepable family. globs name what may go, resolved fresh on every call (a test
// reseeding debris between runs must see the current tree, not a snapshot).
type target struct {
	name string
	doc  string
	// globs are absolute filepath.Glob patterns. A match may be a file or a directory; a directory is
	// removed whole, aged by the newest mtime anywhere under it.
	globs func() []string
}

// targets are the harness's own debris plus the Claude Code CLI's per-session runtime state — this
// repo's own .gitignore is the inventory of what the CLI writes at the config dir it treats as home.
func targets() []target {
	g := func(parts ...string) string { return filepath.Join(append([]string{paths.ConfigDir()}, parts...)...) }
	s := func(parts ...string) string { return filepath.Join(append([]string{paths.StateDir()}, parts...)...) }
	return []target{
		{
			name: "dispatch-log", doc: "state/harness/dispatch: ask records kept for --log/--retry (newest 200 already kept)",
			globs: func() []string { return []string{s("dispatch", "*.json")} },
		},
		{
			name: "memory-state", doc: "state/harness/memory: harvest logs, session stamps, harvest receipts, recall-seen sets, watcher claims",
			globs: func() []string {
				return []string{
					s("memory", "harvest-*.log"), s("memory", "stamp-*"),
					s("memory", "harvest-result-*"), s("memory", "recall-seen", "*"),
					s("memory", "watch-*"),
				}
			},
		},
		{
			name: "hook-state", doc: "per-session hook flags under state/harness: checkpoint/, simplified/, stop-format/, agents-inflight markers",
			globs: func() []string {
				return []string{
					s("checkpoint", "*"), s("simplified", "*"), s("stop-format", "*.json"), s("agents-inflight", "*"),
				}
			},
		},
		{
			// router.json/router.secret are the DAEMON's live state (removed by the daemon itself on
			// exit); only its log is debris, and an active router keeps rewriting it past any cutoff.
			name: "router-log", doc: "router.log (daemon stdio; router.json/secret are live state and never swept)",
			globs: func() []string { return []string{s("router.log")} },
		},
		{
			name: "settings-backups", doc: "settings.json.bak-<timestamp>, written before every settings merge",
			globs: func() []string { return []string{g("settings.json.bak-*")} },
		},
		{
			// Every plugin update leaves the previous version dir behind; only the installPath in
			// installed_plugins.json is live, so the rest is rebuildable debris (a reinstall re-fetches it).
			name: "plugin-versions", doc: "plugins/cache/<marketplace>/<plugin>/<version> dirs no longer named by installed_plugins.json",
			globs: stalePluginVersions,
		},
		{
			// jobs/, file-history/, session-env/, paste-cache/, shell-snapshots/, todos/ are the Claude
			// Code CLI's own per-session debris (see .gitignore); nothing in this binary prunes them.
			name: "cli-state", doc: "Claude Code CLI per-session debris: jobs/, file-history/, session-env/, paste-cache/, shell-snapshots/, todos/",
			globs: func() []string {
				return []string{
					g("jobs", "*"), g("file-history", "*"), g("session-env", "*"), g("paste-cache", "*"),
					g("shell-snapshots", "*"), g("todos", "*"),
				}
			},
		},
		{
			name: "cli-caches", doc: "Claude Code CLI rebuildable caches/logs: cache/, image-cache/, debug/, statsig/, logs/, usage-data/, stats-cache.json, mcp-needs-auth-cache.json",
			globs: func() []string {
				return []string{
					g("cache", "*"), g("image-cache", "*"), g("debug", "*"), g("statsig", "*"),
					g("logs", "*"), g("usage-data", "*"), g("stats-cache.json"), g("mcp-needs-auth-cache.json"),
				}
			},
		},
	}
}

// Run is `claude-code clean`: dry-run unless --apply.
func Run(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(errw)
	what := fs.String("what", "", "comma-separated targets (default: every target); --list names them")
	olderThan := fs.String("older-than", "30d", "sweep only entries untouched this long (e.g. 30d, 12h)")
	apply := fs.Bool("apply", false, "actually delete — without it nothing is removed")
	list := fs.Bool("list", false, "print the targets and exit")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	all := targets()
	if *list {
		for _, t := range all {
			fmt.Fprintf(out, "%-16s %s\n", t.name, t.doc)
		}
		return 0
	}
	age, err := parseAge(*olderThan)
	if err != nil {
		fmt.Fprintln(errw, "clean:", err)
		return 2
	}
	selected, err := selectTargets(all, *what)
	if err != nil {
		fmt.Fprintln(errw, "clean:", err)
		return 2
	}

	cutoff := time.Now().Add(-age)
	var totalFiles int
	var totalBytes int64
	for _, t := range selected {
		hits, size := pruneAged(t.globs(), cutoff, *apply)
		if len(hits) == 0 {
			continue
		}
		totalFiles += len(hits)
		totalBytes += size
		verb := "would remove"
		if *apply {
			verb = "removed"
		}
		fmt.Fprintf(errw, "%-16s %s %d entr%s (%s)\n", t.name, verb, len(hits), pluralWord(len(hits), "y", "ies"), humanBytes(size))
	}
	if totalFiles == 0 {
		fmt.Fprintf(errw, "clean: nothing older than %s in %d target(s) — already clean\n", *olderThan, len(selected))
		return 0
	}
	if *apply {
		fmt.Fprintf(errw, "\nreclaimed %s across %d entries. Credentials, settings, and the memory bank were not touched.\n",
			humanBytes(totalBytes), totalFiles)
		return 0
	}
	fmt.Fprintf(errw, "\ndry run — nothing deleted. %s across %d entries would go; re-run with --apply.\n",
		humanBytes(totalBytes), totalFiles)
	return 0
}

// pruneAged removes every glob match older than cutoff (a directory whole, by its newest mtime).
// Size is measured BEFORE removal — dirSize on an already-deleted path always reads 0.
func pruneAged(patterns []string, cutoff time.Time, apply bool) ([]string, int64) {
	var hit []string
	var size int64
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			mt, ok := newestMTime(m)
			if !ok || !mt.Before(cutoff) {
				continue
			}
			if strings.HasSuffix(m, ".lock") {
				continue // live bookkeeping, never debris
			}
			sz := dirSize(m)
			if apply {
				if err := os.RemoveAll(m); err != nil {
					continue
				}
			}
			size += sz
			hit = append(hit, m)
		}
	}
	sort.Strings(hit)
	return hit, size
}

// parseAge accepts Go durations plus a `d` (days) suffix, which time.ParseDuration lacks.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad --older-than %q (want e.g. 30d or 12h)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad --older-than %q (want e.g. 30d or 12h)", s)
	}
	return d, nil
}

// selectTargets resolves --what. Empty selects every target; an unknown name is an error rather than
// a silent no-op, so a typo can never read as "nothing to clean".
func selectTargets(all []target, what string) ([]target, error) {
	if strings.TrimSpace(what) == "" {
		return all, nil
	}
	byName := map[string]target{}
	for _, t := range all {
		byName[t.name] = t
	}
	var out []target
	for _, name := range strings.Split(what, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown target %q (see `claude-code clean --list`)", name)
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--what named no targets")
	}
	return out, nil
}

// newestMTime is the most recent mtime at or under p — "untouched for N days" must mean the whole
// subtree, since a directory's own mtime misses in-place edits to files already inside it.
func newestMTime(p string) (time.Time, bool) {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}, false
	}
	if !st.IsDir() {
		return st.ModTime(), true
	}
	newest := st.ModTime()
	_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry must not abort the walk
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, true
}

// dirSize sums a file's size, or every file under a directory. Best-effort: an unreadable entry counts 0.
func dirSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if !st.IsDir() {
		return st.Size()
	}
	var total int64
	_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree contributes 0, never aborts the walk
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// humanBytes renders a byte count compactly in binary units (1 decimal past B).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", f/unit)
}

// pluralWord picks one or many for n.
func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// stalePluginVersions lists plugin cache version dirs that installed_plugins.json no longer points at. An unreadable
// manifest yields nothing: with no record of what is live, nothing in the cache may be called stale.
func stalePluginVersions() []string {
	root := filepath.Join(paths.ConfigDir(), "plugins")
	raw, err := os.ReadFile(filepath.Join(root, "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var manifest struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(raw, &manifest) != nil || len(manifest.Plugins) == 0 {
		return nil
	}
	live := map[string]bool{}
	for _, entries := range manifest.Plugins {
		for _, e := range entries {
			live[filepath.Clean(e.InstallPath)] = true
		}
	}
	versions, _ := filepath.Glob(filepath.Join(root, "cache", "*", "*", "*"))
	var stale []string
	for _, v := range versions {
		if fi, err := os.Stat(v); err == nil && fi.IsDir() && !live[filepath.Clean(v)] {
			stale = append(stale, v)
		}
	}
	return stale
}
