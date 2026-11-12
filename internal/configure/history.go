package configure

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// history.go keeps a git-backed shadow history of the config files several tools co-own (agent
// CLIs, Claude Code itself, and the user), so a change made two runs ago stays recoverable.
//
// ONE repo under HistoryDir() (paths.StateDir()/history, gitignored) holds the config plane as flat labeled
// copies. Commit subjects are prefixed "config: " and Log/Diff filter on that prefix (git log --grep).
// Memory history is NOT stored here; it has its own git repo at ~/.claude/memory. Every git call runs
// through proc.RunGit's sanitized environment, so a snapshot taken inside a session never touches that
// session's OWN repo.

// HistoryFile is one watched file: Label names it in the store's config directory (unique), Path is its disk location.
type HistoryFile struct {
	Label, Path string
	Read        func() ([]byte, error) // when set, replaces reading Path; absence is fs.ErrNotExist
}

// HistoryStore is the shadow history repo. Dir is created and `git init`-ed on first use.
type HistoryStore struct{ Dir string }

// HistorySkip records a file held back from a snapshot, with the reason a reader needs to act on it.
type HistorySkip struct{ Label, Reason string }

// SnapshotResult reports one Snapshot call; Commit is "" when nothing changed — the caller must not report a snapshot.
type SnapshotResult struct {
	Commit  string
	Changed []string
	Absent  []string
	Skipped []HistorySkip
}

// HistoryDir is the shadow repo's home under the gitignored state dir, so its commits never reach the tracked tree.
func HistoryDir() string { return filepath.Join(paths.StateDir(), "history") }

// plane is the store's one subdirectory and the commit-subject prefix Log/Diff filter on.
const plane = "config"

// configFiles are the config files whose ownership is CONTESTED: a tool other than the one that last
// wrote one of these can rewrite it. The provider overlay and pins are read from the vault; its secrets
// bucket is never read here — textutil.CredentialRE is only the backstop for a credential leaked into one of these.
func configFiles() []HistoryFile {
	cfg := paths.ConfigDir()
	files := []HistoryFile{
		{Label: "settings.json", Path: filepath.Join(cfg, "settings.json")},
		{Label: "settings.local.json", Path: filepath.Join(cfg, "settings.local.json")},
		{Label: "providers.json", Read: providers.OverlayDoc},
		{Label: "provider.env", Read: providers.RenderPins},
		{Label: "claude.json", Path: paths.ClaudeJSON()},
	}
	return append(files, agentConfigs()...)
}

// agentConfigs are the agent CLIs' own on-disk configs. claude-code writes the codex and gemini ones
// (render/MCP registration) and never opencode's (routing goes via env vars / `-c` flags, internal/agent/launch.go);
// the user or the agent's own `login` can rewrite any of them, so a change here is a contested one.
func agentConfigs() []HistoryFile {
	var out []HistoryFile
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	if codexCfg, err := paths.CodexConfigPath(""); err == nil {
		out = append(out, HistoryFile{Label: "codex-config.toml", Path: codexCfg})
	}
	// opencode reads $XDG_CONFIG_HOME or ~/.config on every OS; os.UserConfigDir is ~/Library/... on macOS.
	cfgHome := cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	out = append(out, HistoryFile{Label: "opencode.json", Path: filepath.Join(cfgHome, "opencode", "opencode.json")})
	geminiHome := os.Getenv("GEMINI_CLI_HOME")
	if geminiHome == "" {
		geminiHome = home
	}
	return append(out,
		HistoryFile{Label: "gemini-settings.json", Path: filepath.Join(geminiHome, ".gemini", "antigravity-cli", "settings.json")},
		HistoryFile{Label: "gemini-hooks.json", Path: filepath.Join(geminiHome, ".gemini", "config", "hooks.json")})
}

// Snapshot copies each of files into the store and commits iff something actually changed. An absent
// file is a FACT (an agent config is often absent on a fresh box), not an error; its appearance and
// its deletion are both recorded, so a revert stays auditable.
func (s *HistoryStore) Snapshot(files []HistoryFile, note string) (SnapshotResult, error) {
	var res SnapshotResult
	// The lock file sits beside Dir, not inside it, so `add -A` never stages it.
	err := paths.WithFileLock(s.Dir+".snapshot", func() error {
		if err := s.ensureRepo(); err != nil {
			return err
		}
		for _, f := range files {
			if err := s.stageFile(f, &res); err != nil {
				return err
			}
		}
		leftover, err := s.dirtyLabels()
		if err != nil || len(leftover) == 0 {
			return err
		}
		res.Changed = mergeLeftover(res, leftover)
		commit, err := s.commit(subject(res, note), body(res))
		if err != nil {
			return err
		}
		res.Commit = commit
		return nil
	})
	return res, err
}

// stageFile copies one watched file into the store (or removes it when absent), recording the outcome in res.
func (s *HistoryStore) stageFile(f HistoryFile, res *SnapshotResult) error {
	data, err := readFile(f)
	dest := filepath.Join(s.Dir, plane, f.Label)
	if err != nil {
		return stageUnreadable(f, dest, err, res)
	}
	// Hold the whole file back, never redact: a silent redaction teaches nobody to fix the leak.
	if loc := textutil.CredentialRE.FindIndex(data); loc != nil {
		res.Skipped = append(res.Skipped, HistorySkip{f.Label,
			fmt.Sprintf("holds a credential-shaped value at byte %d — not committed", loc[0])})
		return dropStale(f, dest, res)
	}
	if prev, err := os.ReadFile(dest); err == nil && bytes.Equal(prev, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return err
	}
	res.Changed = append(res.Changed, f.Label)
	return nil
}

// dropStale removes a held-back file's old store copy so the log never shows it as current.
func dropStale(f HistoryFile, dest string, res *SnapshotResult) error {
	err := os.Remove(dest)
	if err == nil {
		res.Changed = append(res.Changed, f.Label)
		return nil
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// stageUnreadable records a watched file that could not be read: absent or skipped, either way removed from the store.
func stageUnreadable(f HistoryFile, dest string, readErr error, res *SnapshotResult) error {
	if !errors.Is(readErr, fs.ErrNotExist) {
		res.Skipped = append(res.Skipped, HistorySkip{f.Label, readErr.Error()})
		return dropStale(f, dest, res)
	}
	res.Absent = append(res.Absent, f.Label)
	// A file that vanished must LEAVE the store, or the log shows it as still present.
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readFile reads f through its Read func when set, else from its Path.
func readFile(f HistoryFile) ([]byte, error) {
	if f.Read != nil {
		return f.Read()
	}
	return os.ReadFile(f.Path)
}

// subject names what moved, so `git log --oneline` alone answers "when did the opus pin change?".
// The leading "config: " is also the Log/Diff filter (git log --grep=^config: ).
func subject(r SnapshotResult, note string) string {
	what := strings.Join(r.Changed, ", ")
	if what == "" {
		what = "removed: " + strings.Join(r.Absent, ", ")
	}
	if note != "" {
		return fmt.Sprintf("%s: %s (%s)", plane, what, note)
	}
	return plane + ": " + what
}

// body records anything held back or absent, so the log answers WHY a file is missing without reading the blob.
func body(r SnapshotResult) string {
	var b strings.Builder
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "SKIPPED %s: %s\n", s.Label, s.Reason)
	}
	for _, l := range r.Absent {
		fmt.Fprintf(&b, "absent: %s\n", l)
	}
	return b.String()
}

// git runs one git command in the store, forcing identity and signing via -c: a missing user.email would fail `commit`.
func (s *HistoryStore) git(args ...string) (string, error) {
	full := append([]string{
		"-C", s.Dir,
		"-c", "user.name=claude-code",
		"-c", "user.email=claude-code@localhost",
		"-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=/dev/null", // a stray global hooksPath must not run against this repo
	}, args...)
	// A tree walk this size is seconds at most; a hung index.lock or filesystem must not hold a hook.
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", full...) // full already carries its own -C, so dir stays ""
}

// ensureRepo creates the store and inits its repo, 0700: these files describe configured providers and workspaces.
func (s *HistoryStore) ensureRepo() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err != nil {
		if _, err := s.git("init", "--quiet", "--initial-branch=main"); err != nil {
			return err
		}
	}
	return nil
}

// dirtyLabels lists the labels with uncommitted content in the config subdirectory (including a deletion).
func (s *HistoryStore) dirtyLabels() ([]string, error) {
	out, err := s.git("status", "--porcelain", "--untracked-files=all", "--", plane)
	if err != nil || out == "" {
		return nil, err
	}
	var labels []string
	for line := range strings.SplitSeq(out, "\n") {
		if _, label, ok := strings.Cut(line, plane+"/"); ok {
			labels = append(labels, label)
		}
	}
	return labels, nil
}

// mergeLeftover adds store changes a failed earlier commit left behind (equal to live now) to Changed, sorted.
func mergeLeftover(r SnapshotResult, leftover []string) []string {
	changed := slices.Clone(r.Changed)
	for _, l := range leftover {
		if !slices.Contains(changed, l) && !slices.Contains(r.Absent, l) {
			changed = append(changed, l)
		}
	}
	sort.Strings(changed)
	return changed
}

// commit stages the whole store (a pathspec restricted to individual LABELS errors outright —
// "pathspec did not match any file(s) known to git" — the moment one watched file has never existed
// on disk, which a first Snapshot hits for whichever files are absent) and records the config
// subdirectory only. The directory pathspec itself is always valid once `add -A` has staged
// at least one file under it, unlike a bare filename pathspec that was never tracked.
func (s *HistoryStore) commit(subject, body string) (string, error) {
	if _, err := s.git("add", "-A"); err != nil {
		return "", err
	}
	args := []string{"commit", "--quiet", "-m", subject}
	if body != "" {
		args = append(args, "-m", body)
	}
	args = append(args, "--", plane)
	if _, err := s.git(args...); err != nil {
		return "", err
	}
	return s.git("rev-parse", "--short", "HEAD")
}

// hasCommits reports whether the store has a repo with a resolvable HEAD; an unborn HEAD is "no commits", else errors.
func (s *HistoryStore) hasCommits() (bool, error) {
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	_, err := s.git("rev-parse", "--verify", "--quiet", "HEAD")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 { // rev-parse --quiet: HEAD does not resolve
		return false, nil
	}
	return false, err
}

// Log returns the last n config commits as "<sha> <date> <subject>" lines, newest first.
func (s *HistoryStore) Log(n int) ([]string, error) {
	born, err := s.hasCommits()
	if err != nil || !born {
		return nil, err
	}
	out, err := s.git("log", fmt.Sprintf("-%d", n), "--grep=^"+plane+": ",
		"--format=%h %ad %s", "--date=format:%Y-%m-%d %H:%M")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// Diff returns one commit's patch: rev when given, else the newest config commit.
func (s *HistoryStore) Diff(rev string) (string, error) {
	if strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("revision %q looks like a git option; pass a commit id from `history log`", rev)
	}
	born, err := s.hasCommits()
	if err != nil {
		return "", err
	}
	if !born {
		return "", fmt.Errorf("no %s commits yet", plane)
	}
	if rev == "" {
		found, err := s.git("log", "--grep=^"+plane+": ", "-1", "--format=%H")
		if err != nil {
			return "", err
		}
		if found == "" {
			return "", fmt.Errorf("no %s commits yet", plane)
		}
		rev = found
	}
	return s.git("show", "--format=%h %ad%n%s%n%n%b", "--date=iso", rev)
}

// SnapshotConfig records the contested config files right before a harness writer overwrites them.
func SnapshotConfig(note string) (SnapshotResult, error) {
	s := &HistoryStore{Dir: HistoryDir()}
	return s.Snapshot(configFiles(), note)
}

// gitTimeout bounds one shadow-history git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// HistoryEntry is one `history log` line: Store.Log formats "%h %Y-%m-%d %H:%M config: <subject>".
type HistoryEntry struct {
	Rev     string `json:"rev"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// ParseHistoryLog splits Store.Log lines into entries, skipping malformed ones.
func ParseHistoryLog(lines []string) []HistoryEntry {
	out := []HistoryEntry{}
	for _, l := range lines {
		f := strings.SplitN(l, " ", 4)
		if len(f) < 4 {
			continue
		}
		out = append(out, HistoryEntry{Rev: f[0], Date: f[1] + " " + f[2], Subject: f[3]})
	}
	return out
}
