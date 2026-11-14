package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// write creates path with contents, making parent dirs as needed.
func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCleanStalePartialLocksOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stale := statePath("plans-scratch", "old.partial.lock")
	fresh := statePath("plans-scratch", "fresh.partial.lock")
	shared := statePath("plans-scratch", "partials.lock")
	other := statePath("plans-scratch", "other.lock")
	for _, path := range []string{stale, fresh, shared, other} {
		write(t, path, "")
		if path != fresh {
			at := time.Now().Add(-25 * time.Hour)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	hook, err := selectTargets(targets(), "hook-state")
	if err != nil {
		t.Fatal(err)
	}
	hits, _, failed := pruneAgedTo(io.Discard, hook[0].globs(), time.Now().Add(-30*24*time.Hour), false, hook[0].keep)
	if failed != 0 || len(hits) != 1 || hits[0] != stale {
		t.Fatalf("dry run = %v, failures %d; want only %s", hits, failed, stale)
	}
	for _, path := range []string{stale, fresh, shared, other} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("dry run changed %s: %v", path, err)
		}
	}
}

// agedDebris are the seeded entries, one or more per target, that a default sweep must remove.
var agedDebris = []string{
	"state/harness/memory/update-20260101-000000.log",
	"state/harness/memory/stamp-sess-aaa",
	"state/harness/memory/update-result-sess-aaa",
	"state/harness/memory/recall-seen/sess-aaa",
	"state/harness/checkpoint/sess-aaa",
	"state/harness/plans-scratch/x.md.dest",
	"state/harness/stop-format/abc123.json",
	"state/harness/agents-inflight/agent-123-aaa",
	"state/harness/router.log",
	"state/harness/dispatch/20260101-aaaa.log",
	"state/harness/backups/settings.json.bak-20260101-000000",
	"backups/.claude.json.backup.1700000000000",
	"jobs/job-aaa/output.log",
	"file-history/f-aaa/v1",
	"session-env/sess-aaa.json",
	"paste-cache/p-aaa.txt",
	"shell-snapshots/snap-aaa.sh",
	"todos/sess-aaa.json",
	"cache/model-x.json",
	"image-cache/img-aaa.png",
	"debug/debug-aaa.log",
	"statsig/stat-aaa.json",
	"logs/log-aaa.log",
	"usage-data/usage-aaa.json",
	"stats-cache.json",
	"mcp-needs-auth-cache.json",
}

// protectedDebris must never be candidates, at any age — credentials, live config, and the memory bank
// (including its archive subtrees, which are never a clean target at all).
var protectedDebris = []string{
	"docs/memory/bank/activeContext.md", "docs/memory/bank/decisionLog.md",
	"docs/memory/bank/archive/sessionHistory-2025-01.md",
	"docs/memory/bank/archive/native/some-project/topic.md",
	"state/vault.db.key", "settings.json", "providers.json", ".credentials.json",
	"state/harness/proxy.json",
}

// seedFiles writes each rel under dir and ages the file (and, when ageDir, its containing dir) to old.
func seedFiles(t *testing.T, dir string, rels []string, contents string, old time.Time, ageDir bool) {
	t.Helper()
	for _, rel := range rels {
		p := filepath.Join(dir, rel)
		write(t, p, contents)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		if !ageDir {
			continue
		}
		if err := os.Chtimes(filepath.Dir(p), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// seedDebris lays down one aged entry per target, plus everything that must survive at any age,
// and returns the isolated config dir.
func seedDebris(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	t.Setenv("TMPDIR", t.TempDir()) // the sandbox target sweeps the temp dir; never the real one
	// The real scan keeps all memory state whenever a same-uid process has an unreadable fd table (host-dependent).
	oldOpen := openMemoryFiles
	openMemoryFiles = func() (map[string]bool, bool) { return map[string]bool{}, true }
	t.Cleanup(func() { openMemoryFiles = oldOpen })
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "foo")
	vaulttest.Secret(t, "API_KEY", "test-value")
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(vault.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	// Age the containing dir too: clean ages a subtree by its NEWEST entry, so a dir that gained a
	// file yesterday must survive even though this seeded file is old.
	seedFiles(t, dir, agedDebris, "x", old, true)
	seedFiles(t, dir, protectedDebris, "keep", old, false)
	return dir
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return err == nil
}

// TestCleanDryRunPreviewsDeletesNothing: --dry-run reports what would go and deletes nothing.
func TestCleanDryRunPreviewsDeletesNothing(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Clean([]string{"--dry-run"}, &out, &errw); code != 0 {
		t.Fatalf("Run --dry-run = %d, want 0", code)
	}
	if !exists(t, filepath.Join(dir, "state/harness/memory/update-20260101-000000.log")) {
		t.Error("--dry-run deleted something — only a run without --dry-run may")
	}
	if !strings.Contains(errw.String(), "--dry-run") {
		t.Errorf("dry run does not name the flag that triggered it:\n%s", errw.String())
	}
}

// TestCleanAppliesByDefault: a bare run deletes aged debris and sizes hits before deletion (never "reclaimed 0 B").
func TestCleanAppliesByDefault(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Clean(nil, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	for _, gone := range []string{
		"state/harness/memory/update-20260101-000000.log", "state/harness/memory/stamp-sess-aaa",
		"state/harness/memory/update-result-sess-aaa", "state/harness/memory/recall-seen/sess-aaa",
		"state/harness/checkpoint/sess-aaa", "state/harness/plans-scratch/x.md.dest",
		"state/harness/stop-format/abc123.json", "state/harness/agents-inflight/agent-123-aaa",
		"state/harness/router.log", "state/harness/dispatch/20260101-aaaa.log",
		"jobs/job-aaa", "file-history/f-aaa", "session-env/sess-aaa.json", "paste-cache/p-aaa.txt",
		"shell-snapshots/snap-aaa.sh", "todos/sess-aaa.json", "cache/model-x.json", "image-cache/img-aaa.png",
		"debug/debug-aaa.log", "statsig/stat-aaa.json", "logs/log-aaa.log", "usage-data/usage-aaa.json",
		"stats-cache.json", "mcp-needs-auth-cache.json",
	} {
		if exists(t, filepath.Join(dir, gone)) {
			t.Errorf("%s survived --apply", gone)
		}
	}
	for _, keep := range []string{
		"docs/memory/bank/activeContext.md", "docs/memory/bank/decisionLog.md",
		"docs/memory/bank/archive/sessionHistory-2025-01.md",
		"docs/memory/bank/archive/native/some-project/topic.md",
		"state/vault.db", "state/vault.db.key", "settings.json", "providers.json", ".credentials.json",
		"state/harness/proxy.json",
	} {
		if !exists(t, filepath.Join(dir, keep)) {
			t.Errorf("clean deleted live config or memory bank content: %s", keep)
		}
	}
	if !strings.Contains(errw.String(), "reclaimed ") || strings.Contains(errw.String(), "reclaimed 0 B") {
		t.Errorf("summary does not report the bytes reclaimed from seeded debris:\n%s", errw.String())
	}
}

// TestCleanRespectsOlderThan: a young file is never swept, whatever the target.
func TestCleanRespectsOlderThan(t *testing.T) {
	dir := seedDebris(t)
	young := filepath.Join(dir, "state/harness/memory/update-20261231-000000.log")
	write(t, young, "x")
	var out, errw bytes.Buffer
	Clean(nil, &out, &errw)
	if !exists(t, young) {
		t.Error("a file inside the age threshold was swept")
	}
	if exists(t, filepath.Join(dir, "state/harness/memory/update-20260101-000000.log")) {
		t.Error("the aged sibling in the same target survived; the young file's survival proves nothing")
	}
}

// TestCleanTargetScopesToOneTarget: naming one target in --target must not sweep the others.
func TestCleanTargetScopesToOneTarget(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "proxy-log"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target proxy-log = %d, want 0", code)
	}
	if exists(t, filepath.Join(dir, "state/harness/router.log")) {
		t.Error("the named target was still not swept")
	}
	if !exists(t, filepath.Join(dir, "state/harness/memory/update-20260101-000000.log")) {
		t.Error("--target proxy-log also swept memory-state")
	}
}

// TestCleanTargetRejectsUnknownTarget: a typo in --target must error, never read as "nothing to clean".
func TestCleanTargetRejectsUnknownTarget(t *testing.T) {
	seedDebris(t)
	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "not-a-real-target"}, &out, &errw); code != 2 {
		t.Errorf("Run --target bogus = %d, want 2", code)
	}
}

// TestCleanWhatFlagRejected: the renamed flag has no --what alias; the old name is now unknown.
func TestCleanWhatFlagRejected(t *testing.T) {
	seedDebris(t)
	var out, errw bytes.Buffer
	if code := Clean([]string{"--what", "router-log"}, &out, &errw); code != 2 {
		t.Errorf("Run --what router-log = %d, want 2 (unknown flag)", code)
	}
	if !strings.Contains(errw.String(), "what") {
		t.Errorf("error does not name the rejected flag:\n%s", errw.String())
	}
}

// TestPruneAgedToFailedRemoval: a removal that errors is counted as a failure and named on errw, never a hit or sized.
func TestPruneAgedToFailedRemoval(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("root and Windows ignore directory modes; the failure premise does not hold")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked-old")
	write(t, filepath.Join(blocked, "x"), "data")
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, p := range []string{filepath.Join(blocked, "x"), blocked} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// Deny write on dir so os.RemoveAll(blocked) cannot unlink blocked from its parent.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	var errw bytes.Buffer
	hit, size, failed := pruneAgedTo(&errw, []string{filepath.Join(dir, "blocked-*")}, time.Now(), true, nil)
	if failed != 1 || len(hit) != 0 || size != 0 {
		t.Errorf("failed=%d hit=%v size=%d, want 1 failure, no hits, 0 bytes", failed, hit, size)
	}
	if !strings.Contains(errw.String(), "could not remove "+blocked) {
		t.Errorf("errw does not name the failed removal:\n%s", errw.String())
	}
	if !exists(t, blocked) {
		t.Error("blocked dir was actually removed despite the read-only parent")
	}
}

// roots reports the directories a target can reach — the credential guard asserts over this.
func (t target) roots() []string {
	var out []string
	for _, g := range t.globs() {
		out = append(out, filepath.Dir(g))
	}
	return out
}

// TestCleanTargetsNeverIncludeCredentialsOrMemoryBank guards the two invariants clean.go states:
// credentials are never touched, and the memory bank (including its archive) is never a target.
func TestCleanTargetsNeverIncludeCredentialsOrMemoryBank(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tg := range targets() {
		for _, pattern := range tg.globs() {
			checkNoVaultMatch(t, tg, pattern)
		}
		for _, root := range tg.roots() {
			checkRootAllowed(t, tg, root)
		}
	}
}

// checkNoVaultMatch fails when a target glob matches the vault or its key.
func checkNoVaultMatch(t *testing.T, tg target, pattern string) {
	t.Helper()
	for _, path := range []string{vault.Path(), vault.Path() + ".key"} {
		matched, err := filepath.Match(pattern, path)
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			t.Errorf("target %q matches protected vault path %q", tg.name, path)
		}
	}
}

// checkRootAllowed fails when a target root resolves under the memory bank, env.d or the memory store.
func checkRootAllowed(t *testing.T, tg target, root string) {
	t.Helper()
	bank := filepath.Join(paths.ConfigDir(), "docs", "memory", "bank")
	envDir := filepath.Join(paths.ConfigDir(), "env.d")
	store := memory.StoreDir()
	if strings.HasPrefix(root, bank) {
		t.Errorf("target %q resolves under the memory bank %q", tg.name, root)
	}
	if strings.HasPrefix(root, envDir) {
		t.Errorf("target %q resolves under env.d %q", tg.name, root)
	}
	// Invariant 2 (updated): no target glob may resolve under the local memory Git store either.
	if root == store || strings.HasPrefix(root, store+string(filepath.Separator)) {
		t.Errorf("target %q resolves under the memory store %q", tg.name, root)
	}
}

func TestPruneAgedNeverSweepsVault(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "apply"}[apply], func(t *testing.T) {
			dir := vaulttest.Use(t)
			vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "a")
			keep := []string{vault.Path(), vault.Path() + ".key", vault.Path() + ".backup"}
			old := time.Now().Add(-400 * 24 * time.Hour)
			ageProtected(t, keep, old)
			debris := filepath.Join(dir, "state", "vault.db-old")
			write(t, debris, "debris")
			if err := os.Chtimes(debris, old, old); err != nil {
				t.Fatal(err)
			}
			if protected(debris) {
				t.Fatal("prefix-sharing sibling must remain sweepable")
			}
			hits, size, _ := pruneAgedTo(io.Discard, []string{filepath.Join(dir, "state", "vault.db*")}, time.Now(), apply, nil)
			if len(hits) != 1 || hits[0] != debris || size != int64(len("debris")) {
				t.Errorf("pruneAgedTo = %v, %d bytes; want only debris", hits, size)
			}
			for _, path := range keep {
				if !exists(t, path) {
					t.Errorf("protected path was deleted: %s", path)
				}
			}
			if exists(t, debris) == apply {
				t.Errorf("debris existence does not match apply=%v", apply)
			}
			assertPinIntact(t, "HARNESS_OPUS_MODEL", "a")
		})
	}
}

// ageProtected backdates each vault path (creating the non-vault ones) and asserts protected() covers it.
func ageProtected(t *testing.T, keep []string, old time.Time) {
	t.Helper()
	for _, path := range keep {
		if path != vault.Path() {
			write(t, path, "keep")
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if !protected(path) {
			t.Errorf("protected(%s) = false", path)
		}
	}
}

// assertPinIntact fails when the vault pin no longer reads want.
func assertPinIntact(t *testing.T, name, want string) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		if pin, _ := tx.Get(vault.Pins, name); pin != want {
			t.Errorf("vault pin changed after prune: %q", pin)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Only plugin version dirs absent from installed_plugins.json are stale; the live installPath is never a target,
// and a missing manifest makes the whole cache off-limits.
func TestStalePluginVersions(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	live := filepath.Join(cfg, "plugins", "cache", "mkt", "plug", "v2")
	old := filepath.Join(cfg, "plugins", "cache", "mkt", "plug", "v1")
	for _, d := range []string{live, old} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := stalePluginVersions(); len(got) != 0 {
		t.Fatalf("no manifest must mean nothing stale, got %v", got)
	}
	manifest := `{"version":1,"plugins":{"plug@mkt":[{"installPath":"` + live + `"}]}}`
	if err := os.WriteFile(filepath.Join(cfg, "plugins", "installed_plugins.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	got := stalePluginVersions()
	if len(got) != 1 || got[0] != old {
		t.Fatalf("stale = %v, want [%s]", got, old)
	}
}

// TestNestedConfig: the nested dir goes only when user settings already carry every key it sets.
func TestNestedConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	write(t, filepath.Join(dir, "settings.json"), `{"outputStyle": "Concise"}`)
	nested := filepath.Join(dir, ".claude")
	write(t, filepath.Join(nested, "settings.json"), `{"outputStyle": "Concise", "prefersReducedMotion": true}`)

	var out, errw bytes.Buffer
	Clean(nil, &out, &errw)
	if !strings.Contains(errw.String(), "settings.json:prefersReducedMotion") {
		t.Fatalf("uncovered key not reported: %s", errw.String())
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatal("nested dir removed while a key was uncovered")
	}

	write(t, filepath.Join(dir, "settings.json"), `{"outputStyle": "Concise", "prefersReducedMotion": true}`)
	write(t, filepath.Join(nested, "agents", "x.md"), "x")
	if n := NestedConfig(); len(n.Keep) != 1 || n.Keep[0] != "agents (not a settings file)" {
		t.Fatalf("non-settings entry not kept: %v", n.Keep)
	}

	os.RemoveAll(filepath.Join(nested, "agents"))
	errw.Reset()
	Clean(nil, &out, &errw)
	if _, err := os.Stat(nested); !os.IsNotExist(err) {
		t.Fatalf("covered nested dir not removed: %s", errw.String())
	}
}

// TestNestedConfigKeepsWorktrees: a linked git worktree under the nested dir is never swept,
// and is reported with its own reason rather than the generic "not a settings file" wording.
func TestNestedConfigKeepsWorktrees(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	write(t, filepath.Join(dir, "settings.json"), `{"outputStyle": "Concise"}`)
	gitFile := filepath.Join(dir, ".claude", "worktrees", "x", ".git")
	write(t, gitFile, "gitdir: /elsewhere/.git/worktrees/x")

	if n := NestedConfig(); !n.OnlyWorktrees() {
		t.Fatalf("worktrees entry not kept with its own reason: %v", n.Keep)
	}

	var out, errw bytes.Buffer
	Clean(nil, &out, &errw)
	if !exists(t, gitFile) {
		t.Error("--apply removed a linked worktree's .git pointer")
	}
	if strings.Contains(errw.String(), "by hand") {
		t.Errorf("worktrees reported as a settings leak: %s", errw.String())
	}
}

// TestBackupsMirrorNeverSwept: the revert mirror survives a sweep; CLI backups age out like any target.
func TestBackupsMirrorNeverSwept(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	mirror := filepath.Join(paths.StateDir(), "backups", "settings.json.bak-20200101-000000")
	cli := filepath.Join(dir, "backups", ".claude.json.backup.1")
	for _, p := range []string{mirror, cli} {
		write(t, p, "x")
		old := time.Now().Add(-90 * 24 * time.Hour)
		_ = os.Chtimes(p, old, old)
	}
	var out, errw bytes.Buffer
	Clean(nil, &out, &errw)
	if _, err := os.Stat(mirror); err != nil {
		t.Errorf("revert mirror swept: %v", err)
	}
	if _, err := os.Stat(cli); !os.IsNotExist(err) {
		t.Errorf("aged CLI backup kept")
	}
}

// TestCleanListShowsBackupsTarget: --list names the renamed settings-backups target (CacheDir, not root).
func TestCleanListShowsBackupsTarget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	var out, errw bytes.Buffer
	if code := Clean([]string{"--list"}, &out, &errw); code != 0 {
		t.Fatalf("Run --list = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "settings-backups") || !strings.Contains(out.String(), "state/harness/backups") {
		t.Errorf("--list missing the renamed target:\n%s", out.String())
	}
	if strings.Contains(out.String(), "settings.json.bak[") {
		t.Errorf("--list still advertises the old root settings.json.bak* doc:\n%s", out.String())
	}
}

// TestCleanOnlyTouchesCacheBackupsNotUnrelatedRootDotfiles: the new CacheDir glob never reaches outside
// state/harness/backups — a root-level file that merely starts with the same prefix survives.
func TestCleanOnlyTouchesCacheBackupsNotUnrelatedRootDotfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	unrelated := filepath.Join(dir, "settings.json.bak-20260101-000000")
	write(t, unrelated, "x")
	var out, errw bytes.Buffer
	Clean(nil, &out, &errw)
	if !exists(t, unrelated) {
		t.Errorf("clean swept a root file outside state/harness/backups: %s", unrelated)
	}
}

// TestSessionsTargetOptInKeepsAutoMemory: transcripts and their <id>/ dirs go only under --target sessions
// (confirmed via --yes, since the test process has no TTY); projects/<slug>/memory and any other sibling
// survive at any age.
func TestSessionsTargetOptInKeepsAutoMemory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	transcript, sessDir := seedAgedSession(t, dir)
	slug := filepath.Join(dir, "projects", "-Users-me-repo")
	memory := filepath.Join(slug, "memory", "MEMORY.md")
	pointer := filepath.Join(slug, "bridge-pointer.json")
	ageUnder(t, slug, time.Now().Add(-40*24*time.Hour), memory, pointer)
	var out, errw bytes.Buffer
	if code := Clean(nil, &out, &errw); code != 0 {
		t.Fatalf("bare Run = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, transcript) || !exists(t, sessDir) {
		t.Fatal("bare sweep removed session transcripts; sessions must be opt-in")
	}
	out.Reset()
	errw.Reset()
	if code := Clean([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, transcript) || exists(t, sessDir) {
		t.Error("--target sessions --yes kept an aged transcript or session dir")
	}
	if !exists(t, memory) || !exists(t, pointer) {
		t.Error("--target sessions swept projects/<slug>/memory or a non-session sibling")
	}
}

// ageUnder writes each file and backdates it and every ancestor up to root: a subtree is aged by its newest entry.
func ageUnder(t *testing.T, root string, old time.Time, files ...string) {
	t.Helper()
	for _, p := range files {
		write(t, p, "x")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		for d := filepath.Dir(p); strings.HasPrefix(d, root); d = filepath.Dir(d) {
			if err := os.Chtimes(d, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// seedAgedSession lays down one aged transcript + session dir under projects/<slug> and returns their paths.
func seedAgedSession(t *testing.T, dir string) (transcript, sessDir string) {
	t.Helper()
	old := time.Now().Add(-40 * 24 * time.Hour)
	slug := filepath.Join(dir, "projects", "-Users-me-repo")
	transcript = filepath.Join(slug, "0ca90491-da59-4309-8468-2e8af18844cd.jsonl")
	sessDir = filepath.Join(slug, "0ca90491-da59-4309-8468-2e8af18844cd", "subagents", "a.jsonl")
	ageUnder(t, slug, old, transcript, sessDir)
	return transcript, sessDir
}

// TestCleanSessionsRefusesWithoutTTYOrYes: --target sessions with no TTY and no --yes deletes nothing
// and exits 2 with the refusal, rather than silently reading as "nothing to clean".
func TestCleanSessionsRefusesWithoutTTYOrYes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	transcript, sessDir := seedAgedSession(t, dir)

	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "sessions"}, &out, &errw); code != 2 {
		t.Fatalf("Run --target sessions = %d, want 2 (refusal is a precondition failure)", code)
	}
	if !exists(t, transcript) || !exists(t, sessDir) {
		t.Error("sessions were deleted without confirmation")
	}
	if !strings.Contains(errw.String(), "--yes") {
		t.Errorf("refusal does not name --yes as the way past it:\n%s", errw.String())
	}
}

// TestCleanSessionsOlderThanStillFilters: --yes bypasses the prompt, not the age filter — a young
// transcript must survive even when sessions are confirmed.
func TestCleanSessionsOlderThanStillFilters(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	young := filepath.Join(dir, "projects", "-Users-me-repo", "11111111-1111-1111-1111-111111111111.jsonl")
	write(t, young, "x")

	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, young) {
		t.Error("--older-than default (30d) did not protect a fresh transcript")
	}
}

// TestCleanSessionsTTYPrompt: on a terminal, only y/yes deletes; n or an empty answer aborts.
func TestCleanSessionsTTYPrompt(t *testing.T) {
	for _, tc := range []struct {
		answer  string
		deleted bool
	}{{"y\n", true}, {"yes\n", true}, {"n\n", false}, {"\n", false}} {
		dir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", dir)
		transcript, _ := seedAgedSession(t, dir)
		oldTTY, oldIn := stdinIsTTY, promptIn
		stdinIsTTY, promptIn = func() bool { return true }, strings.NewReader(tc.answer)
		var out, errw bytes.Buffer
		Clean([]string{"--target", "sessions"}, &out, &errw)
		stdinIsTTY, promptIn = oldTTY, oldIn
		if got := !exists(t, transcript); got != tc.deleted {
			t.Errorf("answer %q: deleted=%v, want %v (%s)", tc.answer, got, tc.deleted, errw.String())
		}
		if !strings.Contains(errw.String(), "[y/N]") {
			t.Errorf("answer %q: prompt not shown: %s", tc.answer, errw.String())
		}
	}
}

// TestProtectedMemoryStoresAndMigrationState: invariant 2 — the memory store (incl. its .git), journals, access logs, the bank lock are never pruned, at any age.
func TestProtectedMemoryStoresAndMigrationState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	old := time.Now().Add(-400 * 24 * time.Hour)
	keep := []string{
		"memory/.git/HEAD", "memory/MEMORY.md", "memory/repo/archive/sessionHistory-2025-01.md",
		"state/harness/memory/journal/txn-1.json",
		"state/harness/memory/access/repo.log", "state/harness/memory/bank.lock",
	}
	for _, rel := range keep {
		p := filepath.Join(dir, rel)
		write(t, p, "keep")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		if !protected(p) {
			t.Errorf("protected(%s) = false", rel)
		}
	}
	for _, rel := range []string{
		"state/harness/memory/stamp-x", "state/harness/memory/update-1.log", "memory-other/x", "state/harness/memory/journal-old",
		"state/harness/memory/migrate-old/manifest.json", "state/harness/memory/journal-old/txn.json",
		"state/harness/memory/access-log/repo.log",
	} {
		if protected(filepath.Join(dir, rel)) {
			t.Errorf("protected(%s) = true: ordinary or prefix-sharing debris must stay sweepable", rel)
		}
	}
	hits, _, _ := pruneAgedTo(io.Discard, []string{filepath.Join(dir, "memory", "*"), filepath.Join(dir, "memory", ".git", "*"),
		filepath.Join(dir, "state", "harness", "memory", "*"), filepath.Join(dir, "state", "harness", "memory", "*", "*")}, time.Now(), true, nil)
	if len(hits) != 0 {
		t.Errorf("pruneAgedTo removed protected paths: %v", hits)
	}
	for _, rel := range keep {
		if !exists(t, filepath.Join(dir, rel)) {
			t.Errorf("%s was deleted", rel)
		}
	}
}

func TestSelectTargetsDedupesRepeatedNames(t *testing.T) {
	all := []target{{name: "hook-state"}, {name: "other"}}
	got, err := selectTargets(all, "hook-state,hook-state")
	if err != nil || len(got) != 1 {
		t.Fatalf("selectTargets = %v, %v; want one target", got, err)
	}
}

// TestCleanHelpDefaultExcludesOptIn: the --target help must not claim a bare sweep runs every target.
func TestCleanHelpDefaultExcludesOptIn(t *testing.T) {
	var out, errw bytes.Buffer
	Clean([]string{"-h"}, &out, &errw)
	if !strings.Contains(out.String()+errw.String(), "opt-in ones") {
		t.Errorf("help does not say opt-in targets are skipped:\n%s%s", out.String(), errw.String())
	}
}

// seedRegistryEntry lays down a sessions/<pid>.json + sessions/<pid>.<hex>.key pair (or any name,
// for the unparsable case) at the given mtime and returns both paths.
func seedRegistryEntry(t *testing.T, dir, name string, mtime time.Time) (jsonPath, keyPath string) {
	t.Helper()
	jsonPath = filepath.Join(dir, "sessions", name+".json")
	keyPath = filepath.Join(dir, "sessions", name+".deadbeef.key")
	for _, p := range []string{jsonPath, keyPath} {
		write(t, p, "x")
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return jsonPath, keyPath
}

// stubPidAlive swaps the liveness probe for alive until the test ends.
func stubPidAlive(t *testing.T, alive func(int) bool) {
	t.Helper()
	old := pidAlive
	pidAlive = alive
	t.Cleanup(func() { pidAlive = old })
}

// session-registry drops a dead pid's pair at any age; a live pid's pair or an unparsable name is kept even when old.
func TestSessionsRegistryKeepsLiveOrUnparsable(t *testing.T) {
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, tc := range []struct {
		name, entry string
		mtime       time.Time
		alive, kept bool
	}{
		{"dead pid old", "4242", old, false, false},
		{"dead pid recent", "4242", time.Now(), false, false},
		{"live pid old", "4242", old, true, true},
		{"unparsable name", "notapid", old, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			stubPidAlive(t, func(int) bool { return tc.alive })
			jsonPath, keyPath := seedRegistryEntry(t, dir, tc.entry, tc.mtime)
			var out, errw bytes.Buffer
			if code := Clean([]string{"--target", "session-registry", "--yes"}, &out, &errw); code != 0 {
				t.Fatalf("Run --target session-registry --yes = %d, want 0: %s", code, errw.String())
			}
			if exists(t, jsonPath) != tc.kept || exists(t, keyPath) != tc.kept {
				t.Errorf("json kept=%v key kept=%v, want %v", exists(t, jsonPath), exists(t, keyPath), tc.kept)
			}
		})
	}
}

// TestSessionsRegistrySplitFromSessionsTarget: bare clean prunes dead registry entries and keeps live
// ones; --target sessions (transcripts only) leaves the registry alone.
func TestSessionsRegistrySplitFromSessionsTarget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	stubPidAlive(t, func(pid int) bool { return pid == 4242 })

	old := time.Now().Add(-40 * 24 * time.Hour)
	deadJSON, deadKey := seedRegistryEntry(t, dir, "1111", old)
	liveJSON, liveKey := seedRegistryEntry(t, dir, "4242", old)
	transcript, sessDir := seedAgedSession(t, dir)

	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions = %d: %s", code, errw.String())
	}
	if exists(t, transcript) || exists(t, sessDir) {
		t.Error("--target sessions kept an aged transcript or session dir next to registry entries")
	}
	if !exists(t, liveJSON) || !exists(t, liveKey) {
		t.Error("--target sessions removed a live pid's registry entry")
	}
	if !exists(t, deadJSON) || !exists(t, deadKey) {
		t.Error("--target sessions removed registry files; they belong to session-registry")
	}
	if code := Clean(nil, &out, &errw); code != 0 {
		t.Fatalf("bare Run = %d: %s", code, errw.String())
	}
	if exists(t, deadJSON) || exists(t, deadKey) {
		t.Error("bare clean left a dead pid's registry entry")
	}
	if !exists(t, liveJSON) || !exists(t, liveKey) {
		t.Error("bare clean removed a live pid's registry entry")
	}
}

func TestCleanCopilotRegistryTargetKeepsLivePid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux runtime dir")
	}
	base := "/run/user/" + strconv.Itoa(os.Getuid())
	if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
		t.Skip("no tmpfs /run/user/<uid>")
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	dir, err := copilot.CopilotRuntimeDir()
	if err != nil {
		t.Skipf("runtime dir unusable here: %v", err)
	}
	live := copilot.CopilotRegistryFile(dir, os.Getpid())
	_ = os.WriteFile(live, []byte("{}"), 0o600)
	t.Cleanup(func() { os.Remove(live) })
	dead := copilot.CopilotRegistryFile(filepath.Dir(live), 1<<22+12346)
	write(t, dead, "{}")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "copilot-registry"}, &out, &errw); code != 0 {
		t.Fatalf("Run = %d: %s", code, errw.String())
	}
	if exists(t, dead) || !exists(t, live) {
		t.Fatalf("dead kept=%v live kept=%v\n%s", exists(t, dead), exists(t, live), out.String())
	}
}

// makeSandbox creates a real `memory sandbox` dir under TMPDIR (empty banks) and ages it.
func makeSandbox(t *testing.T, age time.Duration) string {
	t.Helper()
	res, err := memory.Sandbox(memory.SandboxOptions{Empty: true})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := filepath.WalkDir(res.Dir, func(p string, _ os.DirEntry, _ error) error { return os.Chtimes(p, old, old) }); err != nil {
		t.Fatal(err)
	}
	return res.Dir
}

// sandboxFixture is a TMPDIR holding aged and young sandboxes plus look-alikes that must survive.
type sandboxFixture struct {
	cfg, vaultPath                       string
	aged, young, fake, bare, link, other string
}

func newSandboxFixture(t *testing.T) sandboxFixture {
	t.Helper()
	stubPidAlive(t, func(int) bool { return false })
	fx := sandboxFixture{cfg: vaulttest.Use(t)}
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "foo")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	fx.aged, fx.young = makeSandbox(t, 40*24*time.Hour), makeSandbox(t, time.Hour)
	if filepath.Dir(fx.aged) != filepath.Clean(tmp) {
		t.Fatalf("sandbox %s not under TMPDIR %s", fx.aged, tmp)
	}
	fx.fake = filepath.Join(tmp, sandboxPrefix+"fake") // right name, no seed commit
	write(t, filepath.Join(fx.fake, "memory", ".git", "HEAD"), "ref: refs/heads/main\n")
	fx.bare = filepath.Join(tmp, sandboxPrefix+"bare") // right name, not a sandbox at all
	write(t, filepath.Join(fx.bare, "notes.txt"), "x")
	fx.link = filepath.Join(tmp, sandboxPrefix+"link") // symlink to a real sandbox
	if err := os.Symlink(fx.aged, fx.link); err != nil {
		t.Fatal(err)
	}
	fx.other = filepath.Join(tmp, "unrelated-dir")
	write(t, filepath.Join(fx.other, "f"), "x")
	fx.vaultPath = vault.Path()
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, p := range []string{fx.fake, fx.bare, fx.link, fx.other, filepath.Join(fx.fake, "memory", ".git", "HEAD"), filepath.Join(fx.bare, "notes.txt"), filepath.Join(fx.other, "f")} {
		_ = os.Chtimes(p, old, old)
	}
	return fx
}

// requireSurvivors reports every path the sweep must have left alone.
func (fx sandboxFixture) requireSurvivors(t *testing.T) {
	t.Helper()
	for _, keep := range []string{fx.young, fx.fake, fx.bare, fx.other, fx.cfg} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
	if _, err := os.Stat(fx.vaultPath); err != nil {
		t.Errorf("vault was removed: %v", err)
	}
	if fi, err := os.Lstat(fx.link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink look-alike was removed or replaced: %v", err)
	}
}

// dryRun previews the sweep and fails unless it reports one entry and removes nothing.
func (fx sandboxFixture) dryRun(t *testing.T) {
	t.Helper()
	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "memory-sandboxes", "--dry-run"}, &out, &errw); code != 0 || !strings.Contains(errw.String(), "would remove 1 entry") {
		t.Fatalf("dry run = %d, want 1 entry:\n%s", code, errw.String())
	}
	if !exists(t, fx.aged) {
		t.Fatal("--dry-run removed a sandbox")
	}
}

// TestCleanMemorySandboxes: aged real sandboxes go (and --dry-run only previews); young ones, look-alikes
// without the seed commit, symlinks and the real memory store and vault survive.
func TestCleanMemorySandboxes(t *testing.T) {
	fx := newSandboxFixture(t)
	fx.dryRun(t)
	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", "memory-sandboxes"}, &out, &errw); code != 0 {
		t.Fatalf("clean = %d:\n%s", code, errw.String())
	}
	if exists(t, fx.aged) {
		t.Error("aged sandbox survived")
	}
	fx.requireSurvivors(t)
}

// TestCleanListNamesMemorySandboxesAndDefaultSweeps: --list shows the target and a bare sweep selects it.
func TestCleanListNamesMemorySandboxesAndDefaultSweeps(t *testing.T) {
	var out, errw bytes.Buffer
	if code := Clean([]string{"--list"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "memory-sandboxes") {
		t.Fatalf("--list = %d, missing memory-sandboxes:\n%s", code, out.String())
	}
	sel, err := selectTargets(targets(), "")
	if err != nil || !slices.ContainsFunc(sel, func(tg target) bool { return tg.name == "memory-sandboxes" }) {
		t.Fatalf("default selection lacks memory-sandboxes (err %v)", err)
	}
}

func TestContractCleanSweeps(t *testing.T) {
	for _, f := range schemas.All() {
		if f.Clean == "" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			path := strings.ReplaceAll(f.Glob(), "*", "contract-fixture")
			if !strings.HasPrefix(path, dir) {
				t.Fatalf("glob %s outside %s", path, dir)
			}
			if got := cleanSweep(t, f.Clean); strings.Contains(got, f.Clean+" ") && strings.Contains(got, "would remove") {
				t.Fatalf("empty tree already reports a removal: %s", got)
			}
			if f.Clean == "dispatch-log" { // clean keeps the newest 200 records at any age: bury the fixture below them
				for i := range 200 {
					writeOldCleanFixture(t, filepath.Join(filepath.Dir(path), fmt.Sprintf("zz-%03d.json", i)))
				}
			}
			writeOldCleanFixture(t, path)
			got := cleanSweep(t, f.Clean)
			if !strings.Contains(got, f.Clean) || !strings.Contains(got, "would remove 1 entr") {
				t.Fatalf("clean --target %s does not sweep %s:\n%s", f.Clean, path, got)
			}
		})
	}
}

func cleanSweep(t *testing.T, target string) string {
	t.Helper()
	var out, errw bytes.Buffer
	if code := Clean([]string{"--target", target, "--dry-run", "--older-than", "30d"}, &out, &errw); code != 0 {
		t.Fatalf("clean exit %d: %s", code, errw.String())
	}
	return out.String() + errw.String()
}

func writeOldCleanFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -400)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
