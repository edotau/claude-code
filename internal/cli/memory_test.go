package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

func TestMemoryRootHonorsHarnessWorktreePointer(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	worktree := t.TempDir()
	pointer := "gitdir: " + filepath.Join(cfg, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", worktree)
	if got := memoryRoot(""); got != worktree {
		t.Fatalf("memoryRoot(\"\") = %q, want harness worktree %q (not cwd %q)", got, worktree, other)
	}
}

// The inverse: CLAUDE_CONFIG_DIR is itself a linked worktree, so its main checkout is not the config dir.
func TestMemoryRootHonorsConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pointer := "gitdir: " + filepath.Join(main, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(cfg, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", cfg)
	if got := memoryRoot(""); got != cfg {
		t.Fatalf("memoryRoot(\"\") = %q, want config worktree %q (not cwd %q)", got, cfg, other)
	}
}

// A CLAUDE_PROJECT_DIR that is not a repo (a launcher stamping $HOME) must not win over the cwd's toplevel.
func TestMemoryRootIgnoresNonRepoProjectDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	t.Chdir(filepath.Join(repo, "sub"))
	want, _ := filepath.EvalSymlinks(repo)

	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Dir(repo)) // the non-repo parent
	if got, _ := filepath.EvalSymlinks(memoryRoot("")); got != want {
		t.Errorf("non-repo CLAUDE_PROJECT_DIR won: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", repo) // a real repo root is honored
	if got, _ := filepath.EvalSymlinks(memoryRoot("")); got != want {
		t.Errorf("repo CLAUDE_PROJECT_DIR not honored: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", cfg) // the config dir itself is the global bank's root
	if got := memoryRoot(""); got != cfg {
		t.Errorf("config dir not honored: got %s", got)
	}
}

// memory migrate: the default dry run writes only a manifest (text and json), --origin rejects a bare value,
// a foreign manifest is refused with exit 2, and `memory update` is refused while maintenance is raised.
func TestMemoryMigrateDryRunApplyAndGuard(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := filepath.Join(cfg, "docs", "memory", "bank")
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bank, "conventions.md"), []byte("# C\n- legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--dry-run"}) })
	if code != 0 || !strings.Contains(out, "global-bank") || !strings.Contains(out, "manifest: ") || !strings.Contains(out, "memory migrate --manifest ") {
		t.Fatalf("dry run: code=%d out=%q", code, out)
	}
	if strings.Contains(out, "- legacy") {
		t.Error("dry run printed file contents")
	}
	manifests, _ := filepath.Glob(filepath.Join(memory.MigrateStateDir(), "manifest-*.json"))
	if len(manifests) != 1 {
		t.Fatalf("manifests after dry run: %v", manifests)
	}
	if _, err := os.Stat(memory.StoreDir()); !os.IsNotExist(err) {
		t.Error("dry run created the store")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // WriteManifest names to the second: a fresh dir avoids the O_EXCL clash
	code, out, _ = captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--dry-run", "--format", "json"}) })
	var m memory.Manifest
	if code != 0 || json.Unmarshal([]byte(out), &m) != nil || m.Schema != memory.ManifestSchema {
		t.Errorf("json dry run: code=%d out=%q", code, out[:min(len(out), 200)])
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--origin", "nope"}) }); code != 2 || !strings.Contains(stderr, "SLUG=PATH") {
		t.Errorf("bare --origin: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--format", "json"}) }); code != 1 {
		t.Errorf("--format json on the default apply: code=%d, want 1", code)
	}
	stale := filepath.Join(t.TempDir(), "stale.json")
	if err := os.WriteFile(stale, []byte(`{"schema":1,"configDir":"/elsewhere","items":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--manifest", stale}) }); code != 2 || !strings.Contains(stderr, "does not match this box") {
		t.Errorf("stale manifest: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--manifest", "/nope"}) }); code != 1 {
		t.Errorf("--manifest of a nonexistent manifest: code=%d, want 1", code)
	}
	if err := memory.SetMaintenance("test"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"update", "--transcript", "/nonexistent", "--cwd", cfg, "--session", "s"})
	})
	if code != 1 || !strings.Contains(stderr, "memory maintenance in progress (test)") {
		t.Errorf("paused update: code=%d stderr=%q", code, stderr)
	}
}

// TestMemorySubcommandsWithoutRepoRejectBareRepoFlag: stats/update/dream/show take no --repo flag —
// their own flag.FlagSet has nothing registered for it, so passing it is a parse error (exit 2).
func TestMemorySubcommandsWithoutRepoRejectBareRepoFlag(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"stats", "--repo", "/x"}, 2},
		{[]string{"update", "--repo", "/x"}, 2},
		{[]string{"dream", "--repo", "/x"}, 2},
		{[]string{"show", "--repo", "/x"}, 2},
	}
	for _, c := range cases {
		code, _, stderr := captureOutput(t, func() int { return cmdMemory(c.args) })
		if code != c.want {
			t.Errorf("cmdMemory(%v) = %d, want %d; stderr=%q", c.args, code, c.want, stderr)
		}
	}
}

// TestMemoryPathHonorsRepoFlag: `memory path --repo <dir>` does take --repo, and exits 0 with the
// bank dir under that repo.
func TestMemoryPathHonorsRepoFlag(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"path", "--repo", repo}) })
	if code != 0 {
		t.Fatalf("memory path --repo %s: code = %d, want 0; stderr=%q", repo, code, stderr)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("memory path --repo: empty output, want a bank dir")
	}
}

// TestCmdMemoryFmt: --check is clean (exit 0) on a well-formed bank, reports the over-width file and
// exits 1 without writing it, and the default pass wraps it and exits 0; the usage string and the
// settle-writer guard both name "fmt".
func TestCmdMemoryFmt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bank := memory.GlobalDir()
	target := filepath.Join(bank, "decisionLog.md")
	longLine := "- " + strings.Repeat("xxxxxxxxxx ", 14) + "xxxxxxxxxx"
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# decisionLog\n\n"+longLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := captureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
	if code != 1 || !strings.Contains(out, target) {
		t.Fatalf("--check over a dirty bank: code=%d out=%q, want 1 naming %s", code, out, target)
	}
	if data, err := os.ReadFile(target); err != nil || !strings.Contains(string(data), longLine) {
		t.Fatalf("--check wrote the file: %q err=%v", data, err)
	}

	code, out, _ = captureOutput(t, func() int { return cmdMemoryFmt(nil) })
	if code != 0 || !strings.Contains(out, "wrapped 1 file") {
		t.Fatalf("default fmt pass: code=%d out=%q, want 0 reporting one wrapped file", code, out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if len([]rune(l)) > memory.BankFmtMaxLine {
			t.Errorf("rewritten decisionLog.md still has an over-width line: %q", l)
		}
	}

	code, out, _ = captureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
	if code != 0 || !strings.Contains(out, "clean") {
		t.Errorf("--check after wrapping: code=%d out=%q, want 0 clean", code, out)
	}

	if !strings.Contains(memoryUsage, "memory fmt [--check]") {
		t.Error("memoryUsage does not list `memory fmt [--check]`")
	}
}

// TestCmdMemoryFmtRefusedWhilePaused: fmt is a settle writer, gated the same as index/promote/update/dream.
func TestCmdMemoryFmtRefusedWhilePaused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := memory.SetMaintenance("test"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureOutput(t, func() int { return cmdMemory([]string{"fmt"}) })
	if code != 1 || !strings.Contains(stderr, "memory maintenance in progress (test)") {
		t.Errorf("paused fmt: code=%d stderr=%q", code, stderr)
	}
}

// Phase 1 safety: a raised guard refuses every archive write (--apply, --restore --apply, --split --apply) at the
// memory layer, exit 1 with the notice, and the store is untouched; dry runs and reports stay ungated.
func TestMemoryArchiveWritesRefusedWhilePaused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	dir := memory.NativeDir(repo)
	if err := os.MkdirAll(memory.NativeArchiveDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"topic.md":              "# T\nbody\n",
		memory.NativeIndexFile:  "- [T](topic.md) — t\n",
		"archive/native/old.md": "# O\nold\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := memory.SetMaintenance("test"); err != nil {
		t.Fatal(err)
	}
	writes := [][]string{
		{"--repo", repo, "topic", "--apply"},
		{"--repo", repo, "--restore", "old", "--apply"},
		{"--repo", repo, "--split", "0", "--apply"},
	}
	for _, args := range writes {
		code, _, stderr := captureOutput(t, func() int { return cmdMemoryArchive(args) })
		if code != 1 || !strings.Contains(stderr, "memory maintenance in progress (test)") {
			t.Errorf("paused archive %v: code=%d stderr=%q", args, code, stderr)
		}
	}
	for name, body := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != body {
			t.Errorf("%s changed under a paused guard: %q err=%v", name, data, err)
		}
	}
	reads := [][]string{
		{"--repo", repo, "topic"},
		{"--repo", repo, "--split", "0"},
		{"--repo", repo},
		{"--repo", repo, "--dupes"},
	}
	for _, args := range reads {
		if code, _, stderr := captureOutput(t, func() int { return cmdMemoryArchive(args) }); code != 0 {
			t.Errorf("read-only archive %v under a paused guard: code=%d stderr=%q", args, code, stderr)
		}
	}
}
