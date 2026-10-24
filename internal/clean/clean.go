// Package clean is the explicit state sweep: `claude-code clean` applies by default and the
// age-out every target protects against is otherwise a LAZY side effect (harvest, hooks, install
// each prune only their own path on their own schedule — nothing ever sweeps the rest). --dry-run
// previews without deleting.
//
// Invariants, in order of importance:
//  1. Credentials are NEVER touched: the vault (state/vault.db), settings.json, .credentials.json.
//  2. The memory store (memory/** — the local memory Git repo incl. .git, raw archives)
//     and migration state (state/harness/memory/{migrate,journal,access}, bank.lock,
//     maintenance) are NEVER targets, not even opt-in — they hold irrecoverable history and in-flight writes.
//  3. The opt-in `--target sessions` target always requires confirmation before it deletes anything:
//     a [y/N] prompt on a TTY, --yes without one.
package clean

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/edotau/claude-code/internal/helptext"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// target is one sweepable family. globs name what may go, resolved fresh on every call (a test
// reseeding debris between runs must see the current tree, not a snapshot).
type target struct {
	name string
	doc  string
	// globs are absolute filepath.Glob patterns. A match may be a file or a directory; a directory is
	// removed whole, aged by the newest mtime anywhere under it.
	globs func() []string
	// ageless targets go at any age: --older-than does not gate them.
	ageless bool
	// optIn targets run only when named in --target, never in a bare sweep.
	optIn bool
	// keep, when set, is consulted per matched entry before age gating; true protects it from removal.
	keep func(path string) bool
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
			name: "settings-backups", doc: ".memory-cache/backups/** (harness) + backups/.claude.json.backup.* (CLI)",
			globs: func() []string {
				return []string{filepath.Join(paths.CacheDir(), "backups", "*"), g("backups", ".claude.json.backup.*")}
			},
			ageless: true, // regenerable from the git-tracked template; nothing in them needs aging out
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
			// Rebuilt by every `agents sync`/install; kept by default because diffing it shows what each vendor reads.
			name: "projections", doc: "state/projections: per-vendor agent/instruction renders (opt-in: --target projections)",
			globs: func() []string { return []string{g("state", "projections", "*")} }, ageless: true, optIn: true,
		},
		{
			// Transcripts feed `claude --resume` and `usage`; opt-in so a bare sweep never drops a session
			// someone meant to resume. projects/<slug>/memory (auto-memory) matches neither glob. The
			// sessions/<pid>.json|.key registry entries are pruned too, but only for dead pids.
			name: "sessions", doc: "projects/<slug> transcripts + dead sessions/<pid> registry entries (opt-in)",
			globs: func() []string {
				return []string{
					g("projects", "*", "*.jsonl"), g("projects", "*", "????????-????-????-????-????????????"),
					g("sessions", "*.json"), g("sessions", "*.key"),
				}
			},
			optIn: true,
			keep:  keepLiveSession,
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

// Run is `claude-code clean`: applies unless --dry-run.
func Run(args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	fs.SetOutput(errw)
	want := fs.String("target", "", "comma-separated targets (default: every target); --list names them")
	olderThan := fs.String("older-than", "30d", "sweep only entries untouched this long (e.g. 30d, 12h)")
	dryRun := fs.Bool("dry-run", false, "preview only — print what would be removed, delete nothing")
	yesFlag := fs.Bool("yes", false, "skip the --target sessions confirmation prompt (required without a TTY)")
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
	selected, err := selectTargets(all, *want)
	if err != nil {
		fmt.Fprintln(errw, "clean:", err)
		return 2
	}

	apply := !*dryRun
	cutoff := time.Now().Add(-age)
	if apply && !confirmSessions(selected, cutoff, *olderThan, *yesFlag, errw) {
		fmt.Fprintln(errw, "clean: sessions not confirmed — nothing deleted")
		return 0
	}
	var totalFiles int
	var totalBytes int64
	// The nested config dir is wrong at any age, so it is checked on every run, outside --target/--older-than.
	if n := NestedConfig(); n.Dir != "" {
		if len(n.Keep) > 0 {
			fmt.Fprintf(errw, "%-16s kept %s: move %s into internal/settings/template/, make install, re-run\n",
				"nested-config", n.Dir, strings.Join(n.Keep, ", "))
		} else if hits, size := pruneAged([]string{n.Dir}, time.Now().Add(time.Hour), apply, nil); len(hits) > 0 {
			totalFiles, totalBytes = len(hits), size
			verb := "would remove"
			if apply {
				verb = "removed"
			}
			fmt.Fprintf(errw, "%-16s %s %s\n", "nested-config", verb, n.Dir)
		}
	}
	for _, t := range selected {
		tcut := cutoff
		if t.ageless {
			tcut = time.Now().Add(time.Hour)
		}
		hits, size := pruneAged(t.globs(), tcut, apply, t.keep)
		if len(hits) == 0 {
			continue
		}
		totalFiles += len(hits)
		totalBytes += size
		verb := "would remove"
		if apply {
			verb = "removed"
		}
		fmt.Fprintf(errw, "%-16s %s %d entr%s (%s)\n", t.name, verb, len(hits), pluralWord(len(hits), "y", "ies"), humanBytes(size))
	}
	if totalFiles == 0 {
		fmt.Fprintf(errw, "clean: nothing older than %s in %d target(s) — already clean\n", *olderThan, len(selected))
		return 0
	}
	if apply {
		fmt.Fprintf(errw, "\nreclaimed %s across %d entries. Credentials, user settings, and the memory bank were not touched.\n",
			humanBytes(totalBytes), totalFiles)
		return 0
	}
	fmt.Fprintf(errw, "\ndry run — nothing deleted. %s across %d entries would go; re-run without --dry-run to apply.\n",
		humanBytes(totalBytes), totalFiles)
	return 0
}

// confirmSessions gates the opt-in "sessions" target behind an explicit yes: a [y/N] prompt on a
// TTY, --yes without one. True when there is nothing to confirm (sessions not selected, or empty)
// or confirmation was given; false means the whole run must abort without deleting anything.
func confirmSessions(selected []target, cutoff time.Time, olderThan string, autoYes bool, errw io.Writer) bool {
	var sess target
	found := false
	for _, t := range selected {
		if t.name == "sessions" {
			sess, found = t, true
		}
	}
	if !found {
		return true
	}
	hits, size := pruneAged(sess.globs(), cutoff, false, sess.keep)
	if len(hits) == 0 {
		return true
	}
	repos := map[string]bool{}
	var transcripts, deadEntries int
	for _, h := range hits {
		dir := filepath.Base(filepath.Dir(h))
		if dir == "sessions" {
			deadEntries++
			continue
		}
		transcripts++
		repos[dir] = true
	}
	fmt.Fprintf(errw, "sessions: %d transcript%s, %d dead-session entr%s (%s) older\n",
		transcripts, pluralWord(transcripts, "", "s"), deadEntries, pluralWord(deadEntries, "y", "ies"), humanBytes(size))
	fmt.Fprintf(errw, "  than %s across %d repo%s\n", olderThan, len(repos), pluralWord(len(repos), "", "s"))
	if autoYes {
		return true
	}
	if !stdinIsTTY() {
		fmt.Fprintln(errw, "clean: refusing --target sessions without a TTY; pass --yes to confirm")
		return false
	}
	fmt.Fprint(errw, "delete them? [y/N] ")
	line, _ := bufio.NewReader(promptIn).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// Test seams for the sessions [y/N] prompt.
var (
	stdinIsTTY           = tui.StdinIsTerminal
	promptIn   io.Reader = os.Stdin
)

// keepLiveSession protects a sessions/<pid>.json|.key registry entry whose process is still running;
// an unparsable pid prefix is kept too, since it is never pruned by Claude Code's own registry writer.
// Transcripts and subagent dirs (not under a "sessions" dir) are never covered by this filter.
func keepLiveSession(p string) bool {
	if filepath.Base(filepath.Dir(p)) != "sessions" {
		return false
	}
	name, _, _ := strings.Cut(filepath.Base(p), ".")
	pid, err := strconv.Atoi(name)
	if err != nil {
		return true
	}
	return pidAlive(pid)
}

// pruneAged removes every glob match older than cutoff (a directory whole, by its newest mtime).
// Size is measured BEFORE removal — dirSize on an already-deleted path always reads 0. keep, when
// non-nil, is consulted per match before removal; a kept entry is never counted or removed.
func pruneAged(patterns []string, cutoff time.Time, apply bool, keep func(string) bool) ([]string, int64) {
	var hit []string
	var size int64
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			mt, ok := newestMTime(m)
			if !ok || !mt.Before(cutoff) {
				continue
			}
			if strings.HasSuffix(m, ".lock") || protected(m) || (keep != nil && keep(m)) {
				continue // live bookkeeping, memory stores/migration state, live sessions: never debris
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

// protected guards the vault, memory store, and migration state behind the sweep globs.
func protected(p string) bool {
	p = filepath.Clean(p)
	vaultPath := filepath.Clean(vault.Path())
	if p == vaultPath || strings.HasPrefix(p, vaultPath+".") {
		return true
	}
	for _, exact := range []string{memory.BankLockPath(), memory.MaintenancePath()} {
		if p == filepath.Clean(exact) {
			return true
		}
	}
	for _, root := range []string{memory.StoreDir(), memory.MigrateStateDir(), memory.JournalDir(), memory.AccessStateDir()} {
		root = filepath.Clean(root)
		if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
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

// selectTargets resolves --target. Empty selects every target; an unknown name is an error rather than
// a silent no-op, so a typo can never read as "nothing to clean".
func selectTargets(all []target, want string) ([]target, error) {
	if strings.TrimSpace(want) == "" {
		var out []target
		for _, t := range all {
			if !t.optIn {
				out = append(out, t)
			}
		}
		return out, nil
	}
	byName := map[string]target{}
	for _, t := range all {
		byName[t.name] = t
	}
	var out []target
	for name := range strings.SplitSeq(want, ",") {
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
		return nil, errors.New("--target named no targets")
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
