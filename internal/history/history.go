// Package history keeps a git-backed shadow history of the config files several tools co-own (vendor
// CLIs, Claude Code itself, and the user), so a change made two runs ago stays recoverable.
//
// ONE repo under state/harness/history (gitignored) carries both "planes" — config files and the
// memory view (tree.go) — as flat labeled copies. A plane's commit subject is prefixed "config: " or
// "memory: ", and Log/Diff filter on that prefix (git log --grep), so the two stay independent
// histories without needing two separate repos. Every git call runs through gitenv.go's sanitized
// environment, so a snapshot taken inside a session never touches that session's OWN repo.
package history

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// HistoryFile is one watched file: Label names it inside the store (flat namespace — a label must be
// unique across BOTH planes), Path is where it lives on disk.
type HistoryFile struct{ Label, Path string }

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

// Dir is the shadow history repo's home: under state/ (gitignored), so these commits never reach the
// harness's own tracked tree.
func Dir() string { return filepath.Join(paths.StateDir(), "history") }

// ConfigFiles are the config files whose ownership is CONTESTED: a tool other than the one that last
// wrote one of these can rewrite it. env.d/secrets.env is deliberately absent BY NAME — secretRE below
// is the backstop for a credential that leaked into one of these, never the policy for secrets.env
// itself, which must never be read into this process at all.
func ConfigFiles() []HistoryFile {
	cfg := paths.ConfigDir()
	files := []HistoryFile{
		{Label: "settings.json", Path: filepath.Join(cfg, "settings.json")},
		{Label: "settings.local.json", Path: filepath.Join(cfg, "settings.local.json")},
		{Label: "providers.json", Path: filepath.Join(paths.EnvDir(), "providers.json")},
		{Label: "provider.env", Path: filepath.Join(paths.EnvDir(), "provider.env")},
		{Label: "claude.json", Path: paths.ClaudeJSON()},
	}
	return append(files, vendorConfigs()...)
}

// vendorConfigs are the vendor CLIs' own on-disk configs. claude-code itself never writes these —
// codex and opencode take their routing via env vars / `-c` flags at launch time, never a file
// (internal/vendors/vendors.go) — but the user or the vendor's own `login` can, so a hand-edit here is
// exactly the kind of contested change this history is for.
func vendorConfigs() []HistoryFile {
	var out []HistoryFile
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	out = append(out, HistoryFile{Label: "codex-config.toml", Path: filepath.Join(codexHome, "config.toml")})
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

// secretRE matches credential-SHAPED values (Databricks PATs, JWTs, vendor keys). A git history is
// durable and gets copied around, so a token committed once is leaked for good; a match holds the
// whole file back rather than redacting it, because a silent redaction teaches nobody to fix the leak.
//
// Every prefix is anchored on \b: unanchored, `sk-` matches MID-WORD inside kebab-case prose
// (a`sk-before-changing-appearance`), which would hold back ordinary memory-bank prose. A guard that
// eats ordinary text is worse than none — its output gets ignored and the tree stops being recorded.
var secretRE = regexp.MustCompile(
	`\bdapi[0-9a-f]{32}` + // Databricks PAT
		`|\beyJ[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}` + // JWT (header.payload)
		`|\bsk-[A-Za-z0-9_-]{24,}` + // OpenAI/Anthropic-style key
		`|\bnvapi-[A-Za-z0-9_-]{24,}` + // NVIDIA
		`|\bgh[pousr]_[A-Za-z0-9]{28,}`) // GitHub

// Snapshot copies each of files into the store and commits iff something in THIS plane actually
// changed. An absent file is a FACT (a vendor config is often absent on a fresh box), not an error;
// its appearance and its deletion are both recorded, so a revert stays auditable. plane names the
// commit subject prefix ("config") and scopes the commit to just these files, so a stray
// uncommitted change left over in the other plane never rides along.
func (s *Store) Snapshot(files []HistoryFile, plane, note string) (Result, error) {
	var res Result
	if err := s.ensureRepo(); err != nil {
		return res, err
	}
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		dest := filepath.Join(s.Dir, plane, f.Label)
		switch {
		case err != nil && os.IsNotExist(err):
			res.Absent = append(res.Absent, f.Label)
			// A file that vanished must LEAVE the store, or the log shows it as still present.
			if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
				return res, err
			}
			continue
		case err != nil:
			res.Skipped = append(res.Skipped, Skip{f.Label, err.Error()})
			continue
		}
		if loc := secretRE.FindIndex(data); loc != nil {
			res.Skipped = append(res.Skipped, Skip{f.Label,
				fmt.Sprintf("holds a credential-shaped value at byte %d — not committed", loc[0])})
			continue
		}
		if prev, err := os.ReadFile(dest); err == nil && bytes.Equal(prev, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return res, err
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return res, err
		}
		res.Changed = append(res.Changed, f.Label)
	}
	sort.Strings(res.Changed)
	dirty, err := s.dirty(plane)
	if err != nil || !dirty {
		return res, err
	}
	commit, err := s.commit(subject(plane, res, note), body(res), plane)
	if err != nil {
		return res, err
	}
	res.Commit = commit
	return res, nil
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

// Log returns plane's last n commits as "<sha> <date> <subject>" lines, newest first.
func (s *Store) Log(n int, plane string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err != nil {
		return nil, nil
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
