package history

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotVaultConfigAndPinDiff(t *testing.T) {
	root := vaulttest.Use(t)
	overlay := `{"providers":{}}`
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "a")
	files := vaultConfigFiles(t)
	s := &Store{Dir: filepath.Join(root, "history")}
	first, err := s.Snapshot(files, "config", "first")
	if err != nil {
		t.Fatal(err)
	}
	if first.Commit == "" || len(first.Changed) != 2 ||
		!slices.Contains(first.Changed, "providers.json") || !slices.Contains(first.Changed, "provider.env") {
		t.Fatalf("Snapshot = %+v, want both vault config files committed", first)
	}
	assertSnapshotMatchesVault(t, s, overlay)
	vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "b")
	second, err := s.Snapshot(files, "config", "changed pin")
	if err != nil {
		t.Fatal(err)
	}
	if second.Commit == "" || !slices.Equal(second.Changed, []string{"provider.env"}) {
		t.Fatalf("Snapshot = %+v, want only provider.env changed", second)
	}
	assertPinDiff(t, s)
}

// assertPinDiff fails unless the latest config diff shows the opus pin moving from a to b.
func assertPinDiff(t *testing.T, s *Store) {
	t.Helper()
	diff, err := s.Diff("", "config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-HARNESS_OPUS_MODEL=a", "+HARNESS_OPUS_MODEL=b"} {
		if !slices.Contains(strings.Split(diff, "\n"), want) {
			t.Errorf("Diff missing %q:\n%s", want, diff)
		}
	}
}

// vaultConfigFiles returns the two vault-backed watched files, failing unless both are present.
func vaultConfigFiles(t *testing.T) []HistoryFile {
	t.Helper()
	var files []HistoryFile
	for _, f := range ConfigFiles() {
		if f.Label == "providers.json" || f.Label == "provider.env" {
			files = append(files, f)
		}
	}
	if len(files) != 2 {
		t.Fatalf("want both vault config labels, got %v", files)
	}
	return files
}

// assertSnapshotMatchesVault fails when a stored snapshot differs from the vault's own rendering.
func assertSnapshotMatchesVault(t *testing.T, s *Store, overlay string) {
	t.Helper()
	pins, err := providers.RenderPins()
	if err != nil {
		t.Fatal(err)
	}
	for label, want := range map[string][]byte{"provider.env": pins, "providers.json": []byte(overlay)} {
		got, err := os.ReadFile(filepath.Join(s.Dir, "config", label))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s snapshot differs from vault rendering", label)
		}
	}
}

func TestSnapshotReadNotExistIsAbsent(t *testing.T) {
	for _, readErr := range []error{fs.ErrNotExist, fmt.Errorf("vault pins: %w", fs.ErrNotExist)} {
		t.Run(readErr.Error(), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "provider.env")
			write(t, path, "HARNESS_OPUS_MODEL=a\n")
			s := &Store{Dir: filepath.Join(root, "history")}
			files := []HistoryFile{{Label: "provider.env", Path: path}}
			if _, err := s.Snapshot(files, "config", "first"); err != nil {
				t.Fatal(err)
			}
			files[0].Read = func() ([]byte, error) { return nil, readErr }
			res, err := s.Snapshot(files, "config", "removed")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Absent, []string{"provider.env"}) || res.Commit == "" {
				t.Fatalf("Snapshot = %+v, want committed absence despite existing Path", res)
			}
			if _, err := os.Stat(filepath.Join(s.Dir, "config", "provider.env")); !os.IsNotExist(err) {
				t.Fatalf("absent callback left the old snapshot: %v", err)
			}
		})
	}
}

// Core contract: Snapshot creates the shadow repo and commits exactly the watched files that changed.
// One label (missing.json) never exists on disk — a regression guard: a pathspec-restricted `git add`
// or `git commit` on an individual label that was never tracked errors outright ("pathspec did not
// match"), which an earlier version of this plane-scoping hit on every real watch list (settings.local
// .json and claude.json are routinely absent). Scoping by the plane's subdirectory instead fixes it.
func TestSnapshotCreatesRepoAndCommitsWatchedFiles(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	write(t, cfg, `{"env":{}}`)
	s := &Store{Dir: filepath.Join(root, "history")}
	files := []HistoryFile{
		{Label: "settings.json", Path: cfg},
		{Label: "missing.json", Path: filepath.Join(root, "missing.json")},
	}

	res, err := s.Snapshot(files, "config", "first")
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit == "" || len(res.Changed) != 1 || res.Changed[0] != "settings.json" {
		t.Fatalf("Snapshot = %+v, want one commit with settings.json changed", res)
	}
	if len(res.Absent) != 1 || res.Absent[0] != "missing.json" {
		t.Errorf("Absent = %v, want [missing.json]", res.Absent)
	}
	assertShadowRepoHolds(t, s, "settings.json")

	again, err := s.Snapshot(files, "config", "unchanged")
	if err != nil {
		t.Fatal(err)
	}
	if again.Commit != "" {
		t.Errorf("unchanged snapshot committed anyway: %+v", again)
	}
}

// assertShadowRepoHolds fails unless the shadow repo exists and the plane's store holds label.
func assertShadowRepoHolds(t *testing.T, s *Store, label string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err != nil {
		t.Errorf("shadow repo not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "config", label)); err != nil {
		t.Errorf("watched file not copied into the store: %v", err)
	}
}

// Credentials must never enter the history: a git log is durable and copied around, so a token
// committed once is leaked for good. The file is SKIPPED loudly with a byte offset, never redacted.
func TestSnapshotSkipsCredentialShapedValueWithReason(t *testing.T) {
	root := t.TempDir()
	leaky := filepath.Join(root, "leaky.json")
	// Assembled at runtime so no real-looking token literal sits in the tree (push protection scans the diff).
	write(t, leaky, `{"token":"`+"dapi"+strings.Repeat("0123456789abcdef", 2)+`"}`)
	s := &Store{Dir: filepath.Join(root, "history")}
	files := []HistoryFile{{Label: "leaky.json", Path: leaky}}

	res, err := s.Snapshot(files, "config", "mixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Label != "leaky.json" {
		t.Fatalf("Skipped = %v, want leaky.json held back", res.Skipped)
	}
	if res.Skipped[0].Reason == "" || !strings.Contains(res.Skipped[0].Reason, "credential-shaped") {
		t.Errorf("Skipped reason = %q, want it to name the credential-shaped match", res.Skipped[0].Reason)
	}
	if res.Commit != "" {
		t.Errorf("a credential-only snapshot must not commit: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "config", "leaky.json")); !os.IsNotExist(err) {
		t.Error("the skipped file was written into the store anyway")
	}
}

// Log must list the commits Snapshot made, newest first, scoped to the plane asked for.
func TestLogListsCommits(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	write(t, cfg, `{"a":1}`)
	s := &Store{Dir: filepath.Join(root, "history")}
	files := []HistoryFile{{Label: "settings.json", Path: cfg}}

	if _, err := s.Snapshot(files, "config", "one"); err != nil {
		t.Fatal(err)
	}
	write(t, cfg, `{"a":2}`)
	if _, err := s.Snapshot(files, "config", "two"); err != nil {
		t.Fatal(err)
	}

	lines, err := s.Log(10, "config")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("Log returned %d lines, want 2: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "two") {
		t.Errorf("newest log line %q does not name the newest note", lines[0])
	}

	// A plane with no commits yet must report an empty log, not an error.
	memLines, err := s.Log(10, "memory")
	if err != nil {
		t.Fatal(err)
	}
	if len(memLines) != 0 {
		t.Errorf("memory plane Log = %v, want none (no memory snapshot taken)", memLines)
	}
}

// A clean git env is the whole point of history.go: a snapshot taken inside a session must never
// touch whichever repo GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE happen to name — only the history store.
func TestSnapshotIgnoresInheritedGitEnv(t *testing.T) {
	decoy := hosttest.Repo(t)
	write(t, filepath.Join(decoy, "tracked.txt"), "original\n")
	hosttest.In(t, decoy, "add", "tracked.txt")
	hosttest.In(t, decoy, "commit", "--quiet", "-m", "seed")
	headBefore := hosttest.In(t, decoy, "rev-parse", "HEAD")
	statusBefore := hosttest.In(t, decoy, "status", "--porcelain")

	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))

	root := t.TempDir()
	cfg := filepath.Join(root, "settings.json")
	write(t, cfg, `{"a":1}`)
	s := &Store{Dir: filepath.Join(root, "history")}
	if _, err := s.Snapshot([]HistoryFile{{Label: "settings.json", Path: cfg}}, "config", ""); err != nil {
		t.Fatal(err)
	}

	if got := hosttest.In(t, decoy, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("decoy repo HEAD moved: %q -> %q", headBefore, got)
	}
	if got := hosttest.In(t, decoy, "status", "--porcelain"); got != statusBefore {
		t.Errorf("decoy repo status changed:\n%q\n%q", statusBefore, got)
	}
}

// A repo with no commits (every watched file absent) must read as empty history, not a git error.
func TestLogAndDiffOnUnbornBranch(t *testing.T) {
	root := t.TempDir()
	s := &Store{Dir: filepath.Join(root, "history")}
	files := []HistoryFile{{Label: "provider.env", Path: filepath.Join(root, "absent.env")}}
	if _, err := s.Snapshot(files, "config", "first"); err != nil {
		t.Fatal(err)
	}
	if lines, err := s.Log(5, "config"); err != nil || len(lines) != 0 {
		t.Fatalf("Log = %v, %v; want empty, nil", lines, err)
	}
	if _, err := s.Diff("", "config"); err == nil || !strings.Contains(err.Error(), "no config commits yet") {
		t.Fatalf("Diff err = %v, want \"no config commits yet\"", err)
	}
}

func TestLogAndDiffSurfaceRepositoryErrors(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "not-a-directory"), "x")
	write(t, filepath.Join(root, "broken-gitdir", ".git"), "gitdir: missing\n")
	missingObject := hosttest.Repo(t)
	write(t, filepath.Join(missingObject, ".git", "HEAD"), strings.Repeat("1", 40)+"\n")
	for name, dir := range map[string]string{
		"stat failure":   filepath.Join(root, "not-a-directory"),
		"probe failure":  filepath.Join(root, "broken-gitdir"),
		"missing object": missingObject,
	} {
		t.Run(name, func(t *testing.T) {
			s := &Store{Dir: dir}
			if lines, err := s.Log(20, "config"); err == nil || len(lines) != 0 {
				t.Errorf("Log = %v, %v; want no lines and a repository error", lines, err)
			}
			if patch, err := s.Diff("", "config"); err == nil || patch != "" || strings.Contains(err.Error(), "no config commits yet") {
				t.Errorf("Diff = %q, %v; want no patch and a repository error, not empty history", patch, err)
			}
		})
	}
}

func TestSnapshotDropsStaleCopyWhenFileTurnsCredentialShaped(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live.json")
	write(t, live, `{"model":"opus"}`)
	s := &Store{Dir: filepath.Join(root, "history")}
	files := []HistoryFile{{Label: "live.json", Path: live}}
	if _, err := s.Snapshot(files, "config", "clean"); err != nil {
		t.Fatal(err)
	}
	write(t, live, `{"token":"`+"dapi"+strings.Repeat("0123456789abcdef", 2)+`"}`)

	res, err := s.Snapshot(files, "config", "leaky")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "config", "live.json")); !os.IsNotExist(err) {
		t.Error("the held-back file's stale copy stayed in the store")
	}
	if res.Commit == "" {
		t.Errorf("dropping the stale copy must be committed: %+v", res)
	}
}

// Regression: the scanner missed shapes the memory commit gate refuses (PEM private keys, sk- bodies of 20-23).
func TestSnapshotHoldsBackMemoryGateShapes(t *testing.T) {
	cases := map[string]string{
		"pem private key": "-----BEGIN " + "RSA PRIVATE KEY-----",
		"short sk body":   "sk-" + strings.Repeat("Fk", 10),
	}
	for name, v := range cases {
		root := t.TempDir()
		leaky := filepath.Join(root, "leaky.json")
		write(t, leaky, `{"k":"`+v+`"}`)
		s := &Store{Dir: filepath.Join(root, "history")}
		res, err := s.Snapshot([]HistoryFile{{Label: "leaky.json", Path: leaky}}, "config", "x")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Skipped) != 1 {
			t.Errorf("%s: not held back (Skipped = %d entries)", name, len(res.Skipped))
		}
	}
}

func TestMain(m *testing.M) {
	code := hosttest.Main(m)
	hosttest.Cleanup()
	os.Exit(code)
}
