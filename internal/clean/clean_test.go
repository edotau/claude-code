package clean

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
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

// seedDebris lays down one aged entry per target, plus everything that must survive at any age,
// and returns the isolated config dir.
func seedDebris(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "foo")
	vaulttest.Secret(t, "API_KEY", "test-value")
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(vault.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"state/harness/memory/harvest-20260101-000000.log",
		"state/harness/memory/stamp-sess-aaa",
		"state/harness/memory/harvest-result-sess-aaa",
		"state/harness/memory/recall-seen/sess-aaa",
		"state/harness/checkpoint/sess-aaa",
		"state/harness/plans-scratch/x.md.dest",
		"state/harness/stop-format/abc123.json",
		"state/harness/agents-inflight/agent-123-aaa",
		"state/harness/router.log",
		"state/harness/dispatch/20260101-aaaa.log",
		"state/harness/backups/settings.json.bak-20260101-000000",
		"state/harness/backups/settings.json.bak",
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
	} {
		p := filepath.Join(dir, rel)
		write(t, p, "x")
		// Age the containing dir too: clean ages a subtree by its NEWEST entry, so a dir that gained a
		// file yesterday must survive even though this seeded file is old.
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Dir(p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// Must never be candidates, at any age — credentials, live config, and the memory bank (including
	// its archive subtrees, which are never a clean target at all).
	for _, rel := range []string{
		"docs/memory/bank/activeContext.md", "docs/memory/bank/decisionLog.md",
		"docs/memory/bank/archive/sessionHistory-2025-01.md",
		"docs/memory/bank/archive/native/some-project/topic.md",
		"state/vault.db.key", "settings.json", "providers.json", ".credentials.json",
		"state/harness/router.json", "state/harness/router.secret",
	} {
		p := filepath.Join(dir, rel)
		write(t, p, "keep")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
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
	if code := Run([]string{"--dry-run"}, &out, &errw); code != 0 {
		t.Fatalf("Run --dry-run = %d, want 0", code)
	}
	if !exists(t, filepath.Join(dir, "state/harness/memory/harvest-20260101-000000.log")) {
		t.Error("--dry-run deleted something — only a run without --dry-run may")
	}
	if !strings.Contains(errw.String(), "--dry-run") {
		t.Errorf("dry run does not name the flag that triggered it:\n%s", errw.String())
	}
}

// TestCleanAppliesByDefault: with no flags, clean deletes aged debris immediately (no --apply needed).
func TestCleanAppliesByDefault(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run(nil, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	for _, gone := range []string{
		"state/harness/memory/harvest-20260101-000000.log", "state/harness/memory/stamp-sess-aaa",
		"state/harness/memory/harvest-result-sess-aaa", "state/harness/memory/recall-seen/sess-aaa",
		"state/harness/checkpoint/sess-aaa", "state/harness/plans-scratch/x.md.dest",
		"state/harness/stop-format/abc123.json", "state/harness/agents-inflight/agent-123-aaa",
		"state/harness/router.log", "state/harness/dispatch/20260101-aaaa.log",
		"state/harness/backups/settings.json.bak-20260101-000000",
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
		"state/harness/router.json", "state/harness/router.secret",
	} {
		if !exists(t, filepath.Join(dir, keep)) {
			t.Errorf("clean deleted live config or memory bank content: %s", keep)
		}
	}
}

// TestCleanApplyReportsNonZeroBytes: a default run must size hits BEFORE deletion — dirSize on an
// already-removed path always reads 0, which would otherwise make every run print "reclaimed 0 B".
func TestCleanApplyReportsNonZeroBytes(t *testing.T) {
	seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run(nil, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if strings.Contains(errw.String(), "reclaimed 0 B") {
		t.Errorf("reported 0 bytes reclaimed despite deleting seeded debris:\n%s", errw.String())
	}
}

// TestCleanRespectsOlderThan: a young file is never swept, whatever the target.
func TestCleanRespectsOlderThan(t *testing.T) {
	dir := seedDebris(t)
	young := filepath.Join(dir, "state/harness/memory/harvest-20261231-000000.log")
	write(t, young, "x")
	var out, errw bytes.Buffer
	Run(nil, &out, &errw)
	if !exists(t, young) {
		t.Error("a file inside the age threshold was swept")
	}
}

// TestCleanTargetScopesToOneTarget: naming one target in --target must not sweep the others.
func TestCleanTargetScopesToOneTarget(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "router-log"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target router-log = %d, want 0", code)
	}
	if exists(t, filepath.Join(dir, "state/harness/router.log")) {
		t.Error("the named target was still not swept")
	}
	if !exists(t, filepath.Join(dir, "state/harness/memory/harvest-20260101-000000.log")) {
		t.Error("--target router-log also swept memory-state")
	}
}

// TestCleanTargetRejectsUnknownTarget: a typo in --target must error, never read as "nothing to clean".
func TestCleanTargetRejectsUnknownTarget(t *testing.T) {
	seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "not-a-real-target"}, &out, &errw); code != 2 {
		t.Errorf("Run --target bogus = %d, want 2", code)
	}
}

// TestCleanWhatFlagRejected: the renamed flag has no --what alias; the old name is now unknown.
func TestCleanWhatFlagRejected(t *testing.T) {
	seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run([]string{"--what", "router-log"}, &out, &errw); code != 2 {
		t.Errorf("Run --what router-log = %d, want 2 (unknown flag)", code)
	}
	if !strings.Contains(errw.String(), "what") {
		t.Errorf("error does not name the rejected flag:\n%s", errw.String())
	}
}

// TestPruneAgedSkipsFailedRemoval: a removal that errors must not be counted as a hit or sized —
// only actual successes may be reported.
func TestPruneAgedSkipsFailedRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes; the failure premise does not hold")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked-old")
	write(t, filepath.Join(blocked, "x"), "data")
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(blocked, "x"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(blocked, old, old); err != nil {
		t.Fatal(err)
	}
	// Deny write on dir so os.RemoveAll(blocked) cannot unlink blocked from its parent.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	hit, size := pruneAged([]string{filepath.Join(dir, "blocked-*")}, time.Now(), true, nil)
	if len(hit) != 0 || size != 0 {
		t.Errorf("pruneAged counted a failed removal: hit=%v size=%d", hit, size)
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
	bank := filepath.Join(paths.ConfigDir(), "docs", "memory", "bank")
	envDir := filepath.Join(paths.ConfigDir(), "env.d")
	store := memory.StoreDir()
	for _, tg := range targets() {
		for _, pattern := range tg.globs() {
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
		for _, root := range tg.roots() {
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
	}
}

func TestPruneAgedNeverSweepsVault(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "apply"}[apply], func(t *testing.T) {
			dir := vaulttest.Use(t)
			vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "a")
			keep := []string{vault.Path(), vault.Path() + ".key", vault.Path() + ".backup"}
			old := time.Now().Add(-400 * 24 * time.Hour)
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
			debris := filepath.Join(dir, "state", "vault.db-old")
			write(t, debris, "debris")
			if err := os.Chtimes(debris, old, old); err != nil {
				t.Fatal(err)
			}
			if protected(debris) {
				t.Fatal("prefix-sharing sibling must remain sweepable")
			}
			hits, size := pruneAged([]string{filepath.Join(dir, "state", "vault.db*")}, time.Now(), apply, nil)
			if len(hits) != 1 || hits[0] != debris || size != int64(len("debris")) {
				t.Errorf("pruneAged = %v, %d bytes; want only debris", hits, size)
			}
			for _, path := range keep {
				if !exists(t, path) {
					t.Errorf("protected path was deleted: %s", path)
				}
			}
			if exists(t, debris) == apply {
				t.Errorf("debris existence does not match apply=%v", apply)
			}
			if err := vault.View(func(tx *vault.Tx) error {
				if pin, _ := tx.Get(vault.Pins, "HARNESS_OPUS_MODEL"); pin != "a" {
					t.Errorf("vault pin changed after prune: %q", pin)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestProtectedExactBoundary: protected() must not treat a differently-named sibling that merely shares
// a prefix (e.g. "memory-other", "migrate-old") as covered — only the root itself or a true subpath.
func TestProtectedExactBoundary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	for _, rel := range []string{"memory-other/x", "state/harness/memory/migrate-old/manifest.json", "state/harness/memory/journal-old/txn.json", "state/harness/memory/access-log/repo.log"} {
		if protected(filepath.Join(dir, rel)) {
			t.Errorf("protected(%s) = true: prefix-sharing sibling must not be guarded as the real root", rel)
		}
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
	write(t, filepath.Join(nested, "settings.local.json"), `{"outputStyle": "Concise", "prefersReducedMotion": true}`)

	var out, errw bytes.Buffer
	Run(nil, &out, &errw)
	if !strings.Contains(errw.String(), "settings.local.json:prefersReducedMotion") {
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
	Run(nil, &out, &errw)
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
	Run(nil, &out, &errw)
	if !exists(t, gitFile) {
		t.Error("--apply removed a linked worktree's .git pointer")
	}
}

// TestBackupsAgeless: settings backups go on any --apply, however fresh.
func TestBackupsAgeless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	fresh := []string{
		filepath.Join(paths.CacheDir(), "backups", "settings.json.bak"),
		filepath.Join(dir, "backups", ".claude.json.backup.1"),
	}
	for _, p := range fresh {
		write(t, p, "x")
	}
	var out, errw bytes.Buffer
	Run(nil, &out, &errw)
	for _, p := range fresh {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("fresh backup kept: %s", p)
		}
	}
}

// TestCleanListShowsBackupsTarget: --list names the renamed settings-backups target (CacheDir, not root).
func TestCleanListShowsBackupsTarget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	var out, errw bytes.Buffer
	if code := Run([]string{"--list"}, &out, &errw); code != 0 {
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
	Run(nil, &out, &errw)
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
	old := time.Now().Add(-40 * 24 * time.Hour)
	slug := filepath.Join(dir, "projects", "-Users-me-repo")
	transcript := filepath.Join(slug, "0ca90491-da59-4309-8468-2e8af18844cd.jsonl")
	sessDir := filepath.Join(slug, "0ca90491-da59-4309-8468-2e8af18844cd", "subagents", "a.jsonl")
	memory := filepath.Join(slug, "memory", "MEMORY.md")
	pointer := filepath.Join(slug, "bridge-pointer.json")
	for _, p := range []string{transcript, sessDir, memory, pointer} {
		write(t, p, "x")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		// Age every ancestor up to the slug: a subtree is aged by its newest entry, dirs included.
		for d := filepath.Dir(p); strings.HasPrefix(d, slug); d = filepath.Dir(d) {
			if err := os.Chtimes(d, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out, errw bytes.Buffer
	if code := Run(nil, &out, &errw); code != 0 {
		t.Fatalf("bare Run = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, transcript) || !exists(t, sessDir) {
		t.Fatal("bare sweep removed session transcripts; sessions must be opt-in")
	}
	out.Reset()
	errw.Reset()
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, transcript) || exists(t, sessDir) {
		t.Error("--target sessions --yes kept an aged transcript or session dir")
	}
	if !exists(t, memory) || !exists(t, pointer) {
		t.Error("--target sessions swept projects/<slug>/memory or a non-session sibling")
	}
}

// seedAgedSession lays down one aged transcript + session dir under projects/<slug> and returns their paths.
func seedAgedSession(t *testing.T, dir string) (transcript, sessDir string) {
	t.Helper()
	old := time.Now().Add(-40 * 24 * time.Hour)
	slug := filepath.Join(dir, "projects", "-Users-me-repo")
	transcript = filepath.Join(slug, "0ca90491-da59-4309-8468-2e8af18844cd.jsonl")
	sessDir = filepath.Join(slug, "0ca90491-da59-4309-8468-2e8af18844cd", "subagents", "a.jsonl")
	for _, p := range []string{transcript, sessDir} {
		write(t, p, "x")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		for d := filepath.Dir(p); strings.HasPrefix(d, slug); d = filepath.Dir(d) {
			if err := os.Chtimes(d, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	return transcript, sessDir
}

// TestCleanSessionsRefusesWithoutTTYOrYes: --target sessions with no TTY and no --yes deletes nothing
// and reports the refusal, rather than silently reading as "nothing to clean".
func TestCleanSessionsRefusesWithoutTTYOrYes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	transcript, sessDir := seedAgedSession(t, dir)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions = %d, want 0 (refusal is not an error exit)", code)
	}
	if !exists(t, transcript) || !exists(t, sessDir) {
		t.Error("sessions were deleted without confirmation")
	}
	if !strings.Contains(errw.String(), "--yes") {
		t.Errorf("refusal does not name --yes as the way past it:\n%s", errw.String())
	}
}

// TestCleanSessionsYesDeletes: --target sessions --yes deletes without any prompt.
func TestCleanSessionsYesDeletes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	transcript, sessDir := seedAgedSession(t, dir)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, transcript) || exists(t, sessDir) {
		t.Error("--target sessions --yes left the aged transcript or session dir behind")
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
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
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
		Run([]string{"--target", "sessions"}, &out, &errw)
		stdinIsTTY, promptIn = oldTTY, oldIn
		if got := !exists(t, transcript); got != tc.deleted {
			t.Errorf("answer %q: deleted=%v, want %v (%s)", tc.answer, got, tc.deleted, errw.String())
		}
		if !strings.Contains(errw.String(), "[y/N]") {
			t.Errorf("answer %q: prompt not shown: %s", tc.answer, errw.String())
		}
	}
}

// TestProtectedMemoryStoresAndMigrationState: invariant 2 — the memory store (incl. its .git), journals, access logs, the bank lock and the maintenance marker are never pruned, at any age.
func TestProtectedMemoryStoresAndMigrationState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	old := time.Now().Add(-400 * 24 * time.Hour)
	keep := []string{
		"memory/.git/HEAD", "memory/MEMORY.md", "memory/repo/archive/sessionHistory-2025-01.md",
		"state/harness/memory/journal/txn-1.json",
		"state/harness/memory/access/repo.log", "state/harness/memory/bank.lock", "state/harness/memory/maintenance",
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
	for _, rel := range []string{"state/harness/memory/stamp-x", "state/harness/memory/harvest-1.log", "memory-other/x", "state/harness/memory/journal-old"} {
		if protected(filepath.Join(dir, rel)) {
			t.Errorf("protected(%s) = true: ordinary debris must stay sweepable", rel)
		}
	}
	hits, _ := pruneAged([]string{filepath.Join(dir, "memory", "*"), filepath.Join(dir, "memory", ".git", "*"),
		filepath.Join(dir, "state", "harness", "memory", "*"), filepath.Join(dir, "state", "harness", "memory", "*", "*")}, time.Now(), true, nil)
	if len(hits) != 0 {
		t.Errorf("pruneAged removed protected paths: %v", hits)
	}
	for _, rel := range keep {
		if !exists(t, filepath.Join(dir, rel)) {
			t.Errorf("%s was deleted", rel)
		}
	}
}
