package clean

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, rel := range []string{
		"state/harness/memory/harvest-20260101-000000.log",
		"state/harness/memory/stamp-sess-aaa",
		"state/harness/memory/harvest-result-sess-aaa",
		"state/harness/memory/recall-seen/sess-aaa",
		"state/harness/checkpoint/sess-aaa",
		"state/harness/simplified/sess-aaa",
		"state/harness/stop-format/abc123.json",
		"state/harness/agents-inflight/agent-123-aaa",
		"state/harness/router.log",
		"settings.json.bak-20260101-000000",
		"settings.json.bak",
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
		"env.d/provider.env", "settings.json", "providers.json", ".credentials.json",
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

// TestCleanDryRunByDefault: the verb reports and deletes nothing unless --apply.
func TestCleanDryRunByDefault(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run(nil, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if !exists(t, filepath.Join(dir, "state/harness/memory/harvest-20260101-000000.log")) {
		t.Error("a dry run deleted something — --apply is the only thing that may")
	}
	if !strings.Contains(errw.String(), "--apply") {
		t.Errorf("dry run does not say how to actually run it:\n%s", errw.String())
	}
}

// TestCleanApplySweepsEveryTarget: --apply removes aged debris in every target, and leaves
// credentials, live config, and the memory bank (archive included) alone.
func TestCleanApplySweepsEveryTarget(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run([]string{"--apply"}, &out, &errw); code != 0 {
		t.Fatalf("Run --apply = %d, want 0", code)
	}
	for _, gone := range []string{
		"state/harness/memory/harvest-20260101-000000.log", "state/harness/memory/stamp-sess-aaa",
		"state/harness/memory/harvest-result-sess-aaa", "state/harness/memory/recall-seen/sess-aaa",
		"state/harness/checkpoint/sess-aaa", "state/harness/simplified/sess-aaa",
		"state/harness/stop-format/abc123.json", "state/harness/agents-inflight/agent-123-aaa",
		"state/harness/router.log", "settings.json.bak-20260101-000000",
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
		"env.d/provider.env", "settings.json", "providers.json", ".credentials.json",
		"state/harness/router.json", "state/harness/router.secret",
	} {
		if !exists(t, filepath.Join(dir, keep)) {
			t.Errorf("clean deleted live config or memory bank content: %s", keep)
		}
	}
}

// TestCleanApplyReportsNonZeroBytes: --apply must size hits BEFORE deletion — dirSize on an
// already-removed path always reads 0, which would otherwise make every --apply run print "reclaimed 0 B".
func TestCleanApplyReportsNonZeroBytes(t *testing.T) {
	seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run([]string{"--apply"}, &out, &errw); code != 0 {
		t.Fatalf("Run --apply = %d, want 0", code)
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
	Run([]string{"--apply"}, &out, &errw)
	if !exists(t, young) {
		t.Error("a file inside the age threshold was swept")
	}
}

// TestCleanWhatScopesToOneTarget: naming one target in --what must not sweep the others.
func TestCleanWhatScopesToOneTarget(t *testing.T) {
	dir := seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run([]string{"--what", "router-log", "--apply"}, &out, &errw); code != 0 {
		t.Fatalf("Run --what router-log --apply = %d, want 0", code)
	}
	if exists(t, filepath.Join(dir, "state/harness/router.log")) {
		t.Error("the named target was still not swept")
	}
	if !exists(t, filepath.Join(dir, "state/harness/memory/harvest-20260101-000000.log")) {
		t.Error("--what router-log also swept memory-state")
	}
}

// TestCleanWhatRejectsUnknownTarget: a typo in --what must error, never read as "nothing to clean".
func TestCleanWhatRejectsUnknownTarget(t *testing.T) {
	seedDebris(t)
	var out, errw bytes.Buffer
	if code := Run([]string{"--what", "not-a-real-target", "--apply"}, &out, &errw); code != 2 {
		t.Errorf("Run --what bogus = %d, want 2", code)
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

	hit, size := pruneAged([]string{filepath.Join(dir, "blocked-*")}, time.Now(), true)
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
	for _, tg := range targets() {
		for _, root := range tg.roots() {
			if strings.HasPrefix(root, bank) {
				t.Errorf("target %q resolves under the memory bank %q", tg.name, root)
			}
			if strings.HasPrefix(root, envDir) {
				t.Errorf("target %q resolves under env.d %q", tg.name, root)
			}
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
	Run([]string{"--apply"}, &out, &errw)
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
	Run([]string{"--apply"}, &out, &errw)
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

	if n := NestedConfig(); len(n.Keep) != 1 || n.Keep[0] != "worktrees (linked git worktrees — never swept)" {
		t.Fatalf("worktrees entry not kept with its own reason: %v", n.Keep)
	}

	var out, errw bytes.Buffer
	Run([]string{"--apply"}, &out, &errw)
	if !exists(t, gitFile) {
		t.Error("--apply removed a linked worktree's .git pointer")
	}
}

// TestBackupsAgeless: settings backups go on any --apply, however fresh.
func TestBackupsAgeless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	fresh := []string{filepath.Join(dir, "settings.json.bak"), filepath.Join(dir, "backups", ".claude.json.backup.1")}
	for _, p := range fresh {
		write(t, p, "x")
	}
	var out, errw bytes.Buffer
	Run([]string{"--apply"}, &out, &errw)
	for _, p := range fresh {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("fresh backup kept: %s", p)
		}
	}
}
