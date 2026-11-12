// clean.go is the explicit state sweep: `claude-code clean` applies by default and the
// age-out every target protects against is otherwise a LAZY side effect (update, hooks, install
// each prune only their own path on their own schedule — nothing ever sweeps the rest). --dry-run
// previews without deleting.
//
// Invariants, in order of importance:
//  1. Credentials are NEVER touched: the vault (state/vault.db), settings.json, .credentials.json.
//  2. The memory store (memory/** — the local memory Git repo incl. .git, raw archives)
//     and migration state (state/harness/memory/{migrate,journal}, bank.lock) are NEVER targets. Access state is protected except the two named orphan logs.
//  3. The opt-in `--target sessions` target always requires confirmation before it deletes anything:
//     a [y/N] prompt on a TTY, --yes without one.
package setup

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
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

// cfgPath and statePath join under the config and state dirs; the target lists below resolve them fresh per call.
func cfgPath(parts ...string) string {
	return filepath.Join(append([]string{paths.ConfigDir()}, parts...)...)
}

func statePath(parts ...string) string {
	return filepath.Join(append([]string{paths.StateDir()}, parts...)...)
}

// targets are the harness's own debris plus the Claude Code CLI's per-session runtime state — this
// repo's own .gitignore is the inventory of what the CLI writes at the config dir it treats as home.
func targets() []target {
	var all []target
	for _, group := range []func() []target{stateTargets, daemonTargets, cliSessionTargets, cliCacheTargets} {
		all = append(all, group()...)
	}
	return all
}

// stateTargets are the harness's dispatch, codex, memory and hook state under state/harness.
func stateTargets() []target {
	s := statePath
	return []target{
		{
			name: "dispatch-log", doc: "state/harness/dispatch: ask records (newest 200 kept), their logs + review-*.diff attachments",
			globs: func() []string {
				return []string{s("dispatch", "*.json"), s("dispatch", "*.log"),
					s("dispatch", "*"+paths.DispatchEventsSuffix), s("dispatch", "review-*.diff")}
			},
			keep: dispatchKeeper(),
		},
		{
			name: "codex-audit", doc: "state/harness/codex: codex routing audit logs (routing-*.jsonl), aged by --older-than",
			globs: func() []string { return []string{s("codex", "routing-*.jsonl")} },
		},
		{
			name: "memory-state", doc: "state/harness/memory: aged session state, worker logs and orphan global/memories access logs",
			globs: func() []string {
				return []string{
					s("memory", "update-*.log"), s("memory", "stamp-*"),
					s("memory", "update-result-*"), s("memory", "recall-seen", "*"),
					s("memory", "watch-*"), s("memory", "count-*"), s("memory", "root-*"),
					s("memory", "dream-*.log"), s("memory", "harvest-*.log"), s("memory", "catchup-*.log"),
					s("memory", "access", "global.log"), s("memory", "access", "memories.log"),
				}
			},
			keep: memoryStateKeeper(),
		},
		{
			name: "hook-state", doc: "per-session hook flags and plan records; obsolete plans-scratch/*.partial.lock age out after one day",
			globs: func() []string {
				return []string{
					s("checkpoint", "*"), s("plans-scratch", "*.dest"), s("stop-format", "*.json"), s("agents-inflight", "*"),
					s("plans-scratch", "*.partial"), s("plans-scratch", "*.partial.lock"),
				}
			},
		},
	}
}

// daemonTargets are the proxy and copilot daemon leftovers plus settings backups and stale plugin versions.
func daemonTargets() []target {
	return []target{
		{
			// proxy.json/router.secret are the DAEMON's live state (removed by the daemon itself on
			// exit); only its log is debris, and an active proxy keeps rewriting it past any cutoff.
			name: "proxy-log", doc: "proxy.log and the pre-rename router.log (daemon stdio; state and secret are never swept)",
			globs: func() []string { return []string{statePath("proxy.log"), statePath("router.log")} },
		},
		{
			// <pid>.json registries in the runtime dir carry a session token; a dead pid's file is garbage at any age.
			name: "copilot-registry", doc: "copilot BYOK registry files of dead pids in the runtime dir (live pids kept)",
			globs: func() []string {
				if g := paths.CopilotRegistryGlob(); g != "" {
					return []string{g}
				}
				return nil
			},
			ageless: true,
			keep:    paths.CopilotRegistryKeep,
		},
		{
			name: "settings-backups", doc: "state/harness/backups/setup/* + backups/.claude.json.backup.* (CLI), aged; the revert mirror is never swept",
			globs: func() []string {
				return []string{statePath("backups", "setup", "*"), cfgPath("backups", ".claude.json.backup.*")}
			}, // the rest of state/harness/backups is the only agent-config history (configure --revert)
		},
		{
			// Every plugin update leaves the previous version dir behind; only the installPath in
			// installed_plugins.json is live, so the rest is rebuildable debris (a reinstall re-fetches it).
			name: "plugin-versions", doc: "plugins/cache/<marketplace>/<plugin>/<version> dirs no longer named by installed_plugins.json",
			globs: stalePluginVersions,
		},
	}
}

// cliSessionTargets are the CLI's per-session debris, projections and the session registry.
func cliSessionTargets() []target {
	g := cfgPath
	return []target{
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
			// Rebuilt by every `agents sync`/install; kept by default because diffing it shows what each agent reads.
			name: "projections", doc: "state/projections: per-vendor agent/instruction renders (opt-in: --target projections)",
			globs: func() []string { return []string{g("state", "projections", "*")} }, ageless: true, optIn: true,
		},
		{
			// A dead pid's registry entry is garbage at any age; live pids and non-numeric names are kept.
			name: "session-registry", doc: "dead sessions/<pid> registry entries (live pids kept)",
			globs: func() []string {
				return []string{g("sessions", "*.json"), g("sessions", "*.key")}
			},
			ageless: true,
			keep:    keepLiveSession,
		},
	}
}

// cliCacheTargets are the opt-in transcripts and the CLI's rebuildable caches.
func cliCacheTargets() []target {
	g := cfgPath
	return []target{
		{
			// Transcripts feed `claude --resume` and `usage`; opt-in so a bare sweep never drops a session
			// someone meant to resume. projects/<slug>/memory (auto-memory) matches neither glob.
			name: "sessions", doc: "projects/<slug> transcripts (opt-in)",
			globs: func() []string {
				return []string{
					g("projects", "*", "*.jsonl"), g("projects", "*", "????????-????-????-????-????????????"),
				}
			},
			optIn: true,
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

// Clean is `claude-code clean`: applies unless --dry-run.
func Clean(args []string, out, errw io.Writer) int {
	fs := helptext.NewFlagSet("clean", helptext.Page{
		Usage: "claude-code clean [flags]",
		About: "Delete caches, run dirs and backups older than --older-than. Credentials,\n" +
			"settings and memory are never touched; --target sessions asks first.\n" +
			"A redundant ~/.claude/.claude is also removed, at any age, unless it holds settings or files to keep.",
	})
	fs.SetOutput(errw)
	want := fs.String("target", "", "comma-separated targets (default: every target except opt-in ones); --list names them")
	olderThan := fs.String("older-than", "30d", "sweep only entries untouched this long (e.g. 30d, 12h); dead-pid registry entries go at any age")
	dryRun := fs.Bool("dry-run", false, "preview only — print what would be removed, delete nothing")
	yesFlag := fs.Bool("yes", false, "skip the --target sessions confirmation prompt (required without a TTY)")
	list := fs.Bool("list", false, "print the targets and exit")
	if code, done := helptext.Parse(fs, args); done {
		return code
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
		return 2
	}
	// The nested config dir is wrong at any age, so it is checked on every run, outside --target/--older-than.
	tot := sweepTotals{}
	reportNested(errw, apply, &tot)
	for _, t := range selected {
		sweepTarget(errw, t, cutoff, apply, &tot)
	}
	printSummary(errw, tot, *olderThan, len(selected), apply)
	if tot.failed > 0 {
		return 1
	}
	return 0
}

// sweepTotals accumulates what a run removed (or would remove) across the nested dir and every target.
type sweepTotals struct {
	files  int
	bytes  int64
	failed int
}

// removeVerb is the report verb for an applied or a dry run.
func removeVerb(apply bool) string {
	if apply {
		return "removed"
	}
	return "would remove"
}

// reportNested sweeps the nested config dir when it is safe, or says why it was kept.
func reportNested(errw io.Writer, apply bool, tot *sweepTotals) {
	n := NestedConfig()
	if n.Dir == "" {
		return
	}
	if len(n.Keep) > 0 {
		keep := slices.DeleteFunc(slices.Clone(n.Keep), func(k string) bool { return k == KeepWorktrees })
		if len(keep) == 0 {
			fmt.Fprintf(errw, "%-16s kept %s: holds linked worktrees\n", "nested-config", n.Dir)
			return
		}
		fmt.Fprintf(errw, "%-16s kept %s: move %s into ~/.claude/settings.json by hand, re-run\n",
			"nested-config", n.Dir, strings.Join(keep, ", "))
		return
	}
	hits, size, failed := pruneAgedTo(errw, []string{n.Dir}, time.Now().Add(time.Hour), apply, nil)
	tot.failed += failed
	if len(hits) > 0 {
		tot.files, tot.bytes = len(hits), size
		fmt.Fprintf(errw, "%-16s %s %s\n", "nested-config", removeVerb(apply), n.Dir)
	}
}

// sweepTarget prunes one target past its cutoff (ageless targets go at any age) and reports it.
func sweepTarget(errw io.Writer, t target, cutoff time.Time, apply bool, tot *sweepTotals) {
	tcut := cutoff
	if t.ageless {
		tcut = time.Now().Add(time.Hour)
	}
	hits, size, failed := pruneAgedTo(errw, t.globs(), tcut, apply, t.keep)
	tot.failed += failed
	if len(hits) == 0 {
		return
	}
	tot.files += len(hits)
	tot.bytes += size
	fmt.Fprintf(errw, "%-16s %s %d entr%s (%s)\n", t.name, removeVerb(apply), len(hits), textutil.PluralWord(len(hits), "y", "ies"), textutil.HumanBytes(size))
	if !apply {
		for _, path := range hits {
			if obsoletePartialLock(path) {
				fmt.Fprintf(errw, "  would remove %s\n", path)
			}
		}
	}
}

// printSummary closes the run: the nothing-to-do line, or the reclaimed / dry-run total.
func printSummary(errw io.Writer, tot sweepTotals, olderThan string, nTargets int, apply bool) {
	switch {
	case tot.files == 0:
		fmt.Fprintf(errw, "clean: nothing older than %s in %d target(s) — already clean\n", olderThan, nTargets)
	case apply:
		fmt.Fprintf(errw, "\nreclaimed %s across %d entries. Credentials, user settings, and the memory bank were not touched.\n",
			textutil.HumanBytes(tot.bytes), tot.files)
	default:
		fmt.Fprintf(errw, "\ndry run — nothing deleted. %s across %d entries would go; re-run without --dry-run to apply.\n",
			textutil.HumanBytes(tot.bytes), tot.files)
	}
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
	hits, size, _ := pruneAgedTo(io.Discard, sess.globs(), cutoff, false, sess.keep)
	if len(hits) == 0 {
		return true
	}
	repos := map[string]bool{}
	for _, h := range hits {
		repos[filepath.Base(filepath.Dir(h))] = true
	}
	fmt.Fprintf(errw, "sessions: %d transcript%s (%s) older\n", len(hits), textutil.PluralWord(len(hits), "", "s"), textutil.HumanBytes(size))
	fmt.Fprintf(errw, "  than %s across %d repo%s\n", olderThan, len(repos), textutil.PluralWord(len(repos), "", "s"))
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

// pidAlive is a var so tests can stub the liveness probe.
// pidAlive keeps a pid when the probe can't answer (off unix proc.Alive is always false), so pruning no-ops there.
var pidAlive = func(pid int) bool { return pid > 0 && (!proc.Alive(os.Getpid()) || proc.Alive(pid)) }

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

// dispatchKeeper protects the files of the newest agent.DispatchKeep() records; an unreadable dir protects all.
func dispatchKeeper() func(string) bool {
	var keep map[string]bool
	var readable, checked bool
	return func(p string) bool {
		if !checked {
			keep, readable = newestDispatches(paths.DispatchDir())
			checked = true
		}
		return !readable || keep[dispatchStem(p)]
	}
}

// newestDispatches returns the stems of the newest agent.DispatchKeep() records in dir; false when dir is unreadable.
func newestDispatches(dir string) (map[string]bool, bool) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, true
	}
	if err != nil {
		return nil, false
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // ids lead with a UTC timestamp, so name order is age order
	keep := map[string]bool{}
	for i := len(names) - 1; i >= 0 && len(keep) < agent.DispatchKeep(); i-- {
		keep[dispatchStem(names[i])] = true
	}
	return keep, true
}

// dispatchStem is the record id a dispatch file belongs to: its name without the record, log or events suffix.
func dispatchStem(p string) string {
	name := filepath.Base(p)
	for _, suffix := range []string{".json", ".log", paths.DispatchEventsSuffix} {
		if stem, ok := strings.CutSuffix(name, suffix); ok {
			return stem
		}
	}
	return name
}

// pruneAgedTo removes every glob match older than cutoff (a directory whole, by its newest mtime).
// Size is measured BEFORE removal; keep, when non-nil, vetoes a match. Failures go to errw and are counted.
func pruneAgedTo(errw io.Writer, patterns []string, cutoff time.Time, apply bool, keep func(string) bool) (hit []string, size int64, failed int) {
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			sz, ok, err := pruneMatch(m, cutoff, apply, keep)
			if err != nil {
				fmt.Fprintf(errw, "clean: could not remove %s: %v\n", m, err)
				failed++
			} else if ok {
				size += sz
				hit = append(hit, m)
			}
		}
	}
	sort.Strings(hit)
	return hit, size, failed
}

func obsoletePartialLock(path string) bool {
	return filepath.Dir(path) == statePath("plans-scratch") && strings.HasSuffix(path, ".partial.lock")
}

// pruneMatch removes (when apply) one glob match that is old enough and not protected; ok is false when it was skipped.
func pruneMatch(m string, cutoff time.Time, apply bool, keep func(string) bool) (size int64, ok bool, err error) {
	if strings.HasSuffix(m, ".lock") {
		if !obsoletePartialLock(m) {
			return 0, false, nil
		}
		cutoff = time.Now().Add(-24 * time.Hour)
	}
	mt, found := newestMTime(m)
	if !found || !mt.Before(cutoff) {
		return 0, false, nil
	}
	if protected(m) || (keep != nil && keep(m)) {
		return 0, false, nil // live bookkeeping, memory stores/migration state, live sessions: never debris
	}
	sz := paths.DirSize(m)
	if apply {
		if err := os.RemoveAll(m); err != nil {
			return 0, false, err
		}
	}
	return sz, true, nil
}

// protected guards the vault and memory store behind the sweep globs.
func protected(p string) bool {
	p = filepath.Clean(p)
	vaultPath := filepath.Clean(vault.Path())
	if p == vaultPath || strings.HasPrefix(p, vaultPath+".") {
		return true
	}
	if p == filepath.Clean(memory.BankLockPath()) {
		return true
	}
	if paths.Within(p, memory.AccessStateDir()) {
		return !orphanAccessLog(p)
	}
	for _, root := range []string{memory.StoreDir(), memory.JournalDir()} {
		if paths.Within(p, root) {
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
	for _, name := range textutil.SplitCSV(want) {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown target %q (see `claude-code clean --list`)", name)
		}
		if !slices.ContainsFunc(out, func(o target) bool { return o.name == name }) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("--target named no targets")
	}
	return out, nil
}

// newestMTime is the most recent mtime at or under p — "untouched for N days" must mean the whole
// subtree, since a directory's own mtime misses in-place edits to files already inside it. ok is false
// when any entry under p cannot be scanned, so the caller keeps it.
func newestMTime(p string) (time.Time, bool) {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}, false
	}
	if !st.IsDir() {
		return st.ModTime(), true
	}
	newest := st.ModTime()
	err = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, false
	}
	return newest, true
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

// Nested is <config dir>/.claude: a session started in the config repo reads it as PROJECT scope over user scope.
type Nested struct {
	Dir   string   // empty when there is no nested dir
	Files []string // entries in Dir, relative
	Keep  []string // why it cannot be removed safely: uncovered settings keys or non-settings entries
}

// nestedLocal is the "always allow" file Claude Code writes into the nested dir; disposable, swept every session.
const nestedLocal = "settings.local.json"

// KeepWorktrees is the Keep reason for Claude Code's own worktree dir: legitimate, never a settings leak.
const KeepWorktrees = "worktrees (linked git worktrees — never swept)"

// OnlyWorktrees reports a nested dir that holds nothing but linked worktrees — doctor passes it.
func (n Nested) OnlyWorktrees() bool {
	return len(n.Keep) == 1 && n.Keep[0] == KeepWorktrees && len(n.Files) == 1
}

// NestedConfig inspects the nested dir; it is removable only when every settings key it holds is already set,
// to the same value, in user settings.json, and it holds nothing but settings*.json.
func NestedConfig() Nested {
	dir := filepath.Join(paths.ConfigDir(), ".claude")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Nested{}
	}
	n := Nested{Dir: dir}
	user := readJSON(filepath.Join(paths.ConfigDir(), "settings.json"))
	for _, e := range entries {
		n.Files = append(n.Files, e.Name())
		n.Keep = append(n.Keep, nestedKeepReasons(dir, e, user)...)
	}
	sort.Strings(n.Keep)
	return n
}

// nestedKeepReasons lists why one nested-dir entry cannot be removed: nothing when it is a settings
// file whose every key already matches user settings.
func nestedKeepReasons(dir string, e os.DirEntry, user map[string]any) []string {
	if e.IsDir() && e.Name() == "worktrees" {
		return []string{KeepWorktrees}
	}
	if e.Name() == nestedLocal {
		return nil
	}
	if e.IsDir() || !strings.HasPrefix(e.Name(), "settings") || filepath.Ext(e.Name()) != ".json" {
		return []string{e.Name() + " (not a settings file)"}
	}
	local := readJSON(filepath.Join(dir, e.Name()))
	if local == nil {
		return []string{e.Name() + " (unreadable JSON)"}
	}
	var keep []string
	for k, v := range local {
		if uv, ok := user[k]; !ok || !reflect.DeepEqual(uv, v) {
			keep = append(keep, e.Name()+":"+k)
		}
	}
	return keep
}

// readJSON returns a file's top-level object, or nil when it is missing or not an object.
func readJSON(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}
