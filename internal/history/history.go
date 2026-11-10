// Package history keeps a git-backed shadow history of the config files several tools co-own (agent
// CLIs, Claude Code itself, and the user), so a change made two runs ago stays recoverable.
//
// ONE repo under Dir() (paths.CacheDir()/history, gitignored) holds the config plane as flat labeled
// copies. Commit subjects are prefixed "config: " and Log/Diff filter on that prefix (git log --grep).
// Memory history is NOT stored here; it has its own git repo at ~/.claude/memory. Every git call runs
// through proc.RunGit's sanitized environment, so a snapshot taken inside a session never touches that
// session's OWN repo.
package history

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// HistoryFile is one watched file: Label names it inside its plane's directory of the store (unique within
// that plane), Path is where it lives on disk.
type HistoryFile struct {
	Label, Path string
	Read        func() ([]byte, error) // when set, replaces reading Path; absence is fs.ErrNotExist
}

// Store is the shadow history repo. Dir is created and `git init`-ed on first use.
type Store struct{ Dir string }

// Skip records a file held back from a snapshot, with the reason a reader needs to act on it.
type Skip struct{ Label, Reason string }

// Result reports what one Snapshot call did for one plane. Commit is "" when nothing changed — the
// caller must not report a snapshot it did not take.
type Result struct {
	Commit  string
	Changed []string
	Absent  []string
	Skipped []Skip
}

// Dir is the shadow history repo's home: under the hidden cache (gitignored), so these commits never
// reach the harness's own tracked tree.
func Dir() string { return filepath.Join(paths.CacheDir(), "history") }

// ConfigFiles are the config files whose ownership is CONTESTED: a tool other than the one that last
// wrote one of these can rewrite it. The provider overlay and pins are read from the vault; its secrets
// bucket is never read here — textutil.CredentialRE is only the backstop for a credential leaked into one of these.
func ConfigFiles() []HistoryFile {
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

// agentConfigs are the agent CLIs' own on-disk configs. claude-code itself never writes these —
// codex and opencode take their routing via env vars / `-c` flags at launch time, never a file
// (internal/agent/launch.go) — but the user or the agent's own `login` can, so a hand-edit here is
// exactly the kind of contested change this history is for.
func agentConfigs() []HistoryFile {
	var out []HistoryFile
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	if codexCfg, err := paths.CodexConfigPath(""); err == nil {
		out = append(out, HistoryFile{Label: "codex-config.toml", Path: codexCfg})
	}
	if cfgHome, err := os.UserConfigDir(); err == nil {
		out = append(out, HistoryFile{Label: "opencode.json", Path: filepath.Join(cfgHome, "opencode", "opencode.json")})
	}
	geminiHome := os.Getenv("GEMINI_CLI_HOME")
	if geminiHome == "" {
		geminiHome = home
	}
	out = append(out, HistoryFile{Label: "gemini-settings.json", Path: filepath.Join(geminiHome, ".gemini", "settings.json")})
	return out
}

// Snapshot copies each of files into the store and commits iff something in THIS plane actually
// changed. An absent file is a FACT (an agent config is often absent on a fresh box), not an error;
// its appearance and its deletion are both recorded, so a revert stays auditable. plane names the
// commit subject prefix ("config") and scopes the commit to just these files, so a stray
// uncommitted change left over in the other plane never rides along.
func (s *Store) Snapshot(files []HistoryFile, plane, note string) (Result, error) {
	var res Result
	// The lock file sits beside Dir, not inside it, so `add -A` never stages it.
	err := paths.WithFileLock(s.Dir+".snapshot", func() error {
		if err := s.ensureRepo(); err != nil {
			return err
		}
		for _, f := range files {
			if err := s.stageFile(f, plane, &res); err != nil {
				return err
			}
		}
		sort.Strings(res.Changed)
		dirty, err := s.dirty(plane)
		if err != nil || !dirty {
			return err
		}
		commit, err := s.commit(subject(plane, res, note), body(res), plane)
		if err != nil {
			return err
		}
		res.Commit = commit
		return nil
	})
	return res, err
}

// stageFile copies one watched file into the store (or removes it when absent), recording the outcome in res.
func (s *Store) stageFile(f HistoryFile, plane string, res *Result) error {
	data, err := readFile(f)
	dest := filepath.Join(s.Dir, plane, f.Label)
	if err != nil {
		return stageUnreadable(f, dest, err, res)
	}
	// Hold the whole file back, never redact: a silent redaction teaches nobody to fix the leak.
	if loc := textutil.CredentialRE.FindIndex(data); loc != nil {
		res.Skipped = append(res.Skipped, Skip{f.Label,
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
func dropStale(f HistoryFile, dest string, res *Result) error {
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

// stageUnreadable records a watched file that could not be read: absent (and removed from the store) or skipped.
func stageUnreadable(f HistoryFile, dest string, readErr error, res *Result) error {
	if !errors.Is(readErr, fs.ErrNotExist) {
		res.Skipped = append(res.Skipped, Skip{f.Label, readErr.Error()})
		return nil
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
// The leading "<plane>: " is also the Log/Diff filter (git log --grep=^<plane>: ).
func subject(plane string, r Result, note string) string {
	what := strings.Join(r.Changed, ", ")
	if what == "" {
		what = "removed: " + strings.Join(r.Absent, ", ")
	}
	if note != "" {
		return fmt.Sprintf("%s: %s (%s)", plane, what, note)
	}
	return plane + ": " + what
}

// body records anything held back or absent, so the log answers WHY a file is missing without
// reading the blob.
func body(r Result) string {
	var b strings.Builder
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "SKIPPED %s: %s\n", s.Label, s.Reason)
	}
	for _, l := range r.Absent {
		fmt.Fprintf(&b, "absent: %s\n", l)
	}
	return b.String()
}

// git runs one git command in the store, with identity and signing forced via -c: the machine's git
// config is not this package's dependency, and a missing user.email makes `commit` fail outright.
func (s *Store) git(args ...string) (string, error) {
	full := append([]string{
		"-C", s.Dir,
		"-c", "user.name=claude-code",
		"-c", "user.email=claude-code@localhost",
		"-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=/dev/null", // a stray global hooksPath must not run against this repo
	}, args...)
	return runGit(full)
}

// ensureRepo creates the store and initializes its repo. 0700: these files describe which providers
// and workspaces are configured even when they hold no token.
func (s *Store) ensureRepo() error {
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

// dirty reports whether plane's own subdirectory has uncommitted content (including a deletion),
// scoped so one plane's check never sees the other plane's files.
func (s *Store) dirty(plane string) (bool, error) {
	out, err := s.git("status", "--porcelain", "--", plane)
	return out != "", err
}

// commit stages the whole store (a pathspec restricted to individual LABELS errors outright —
// "pathspec did not match any file(s) known to git" — the moment one watched file has never existed
// on disk, which every plane's first Snapshot hits for whichever files are absent) and records
// plane's subdirectory only: `git commit -- <plane>` limits the COMMIT to that one directory even if
// something else is sitting staged, which is what makes per-plane commits safe to interleave inside
// one shared repo and index. The directory pathspec itself is always valid once `add -A` has staged
// at least one file under it, unlike a bare filename pathspec that was never tracked.
func (s *Store) commit(subject, body, plane string) (string, error) {
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

// hasCommits reports whether the store has a repo with a resolvable HEAD. A missing store or an unborn
// HEAD is "no commits"; any other failure (an unreadable .git, a corrupt HEAD) is a real error.
func (s *Store) hasCommits() (bool, error) {
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

// Log returns plane's last n commits as "<sha> <date> <subject>" lines, newest first.
func (s *Store) Log(n int, plane string) ([]string, error) {
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

// Diff returns one commit's patch: rev when given, else plane's newest commit.
func (s *Store) Diff(rev, plane string) (string, error) {
	if rev == "" {
		born, err := s.hasCommits()
		if err != nil {
			return "", err
		}
		if !born {
			return "", fmt.Errorf("no %s commits yet", plane)
		}
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
func SnapshotConfig(note string) (Result, error) {
	s := &Store{Dir: Dir()}
	return s.Snapshot(ConfigFiles(), "config", note)
}

// One exec seam for the store, via internal/proc. The environment handling there must never
// drift: getting it wrong is silently destructive (a retargeted repo gets `add -A` + `commit` run
// against it).

// gitTimeout bounds one shadow-history git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// runGit executes git via the shared proc seam. full is the complete argv after "git",
// already carrying its own -C: dir stays "" here so proc does not add a second one.
func runGit(full []string) (string, error) {
	// A tree walk this size is seconds at most; a hung index.lock or filesystem must not hold a hook.
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", full...)
}

// LogEntry is one `history log` line: Store.Log formats "%h %Y-%m-%d %H:%M <plane>: <subject>".
type LogEntry struct {
	Rev     string `json:"rev"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// ParseLog splits Store.Log lines into entries, skipping malformed ones.
func ParseLog(lines []string) []LogEntry {
	out := []LogEntry{}
	for _, l := range lines {
		f := strings.SplitN(l, " ", 4)
		if len(f) < 4 {
			continue
		}
		out = append(out, LogEntry{Rev: f[0], Date: f[1] + " " + f[2], Subject: f[3]})
	}
	return out
}
