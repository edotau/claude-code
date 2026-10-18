package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// manifestPathFromDryRun runs a dry run migrate and extracts the manifest path it printed.
func manifestPathFromDryRun(t *testing.T) string {
	t.Helper()
	code, out, _ := captureOutput(t, func() int { return cmdMemoryMigrate(nil) })
	if code != 0 {
		t.Fatalf("dry run exit = %d, out=%q", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "manifest: "); ok {
			return p
		}
	}
	t.Fatalf("no manifest line in dry run output: %q", out)
	return ""
}

// Phase 1 safety: a full dry-run -> apply round trip through the CLI actually imports and commits.
func TestMemoryMigrateApplyHappyPathCompletes(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := memory.GlobalDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bank, "conventions.md"), []byte("# C\n- happy path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := manifestPathFromDryRun(t)

	code, out, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--apply", manifestPath}) })
	if code != 0 || !strings.Contains(out, "complete") {
		t.Fatalf("apply: code=%d out=%q stderr=%q", code, out, stderr)
	}
	if !strings.Contains(out, "store live: ") || !strings.Contains(out, "link linked") {
		t.Errorf("a complete apply must cut over: link native dirs and report the live store: %q", out)
	}
	dest := filepath.Join(memory.StoreDir(), "conventions.md")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("imported file missing at %s: %v", dest, err)
	}
	if !strings.Contains(string(data), "happy path") {
		t.Errorf("imported content = %q", data)
	}
}

// Phase 1 safety: a live watcher claim refuses the CLI apply with exit 2, and nothing is copied.
func TestMemoryMigrateApplyRefusesWhileNotQuiesced(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := memory.GlobalDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bank, "conventions.md"), []byte("# C\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := manifestPathFromDryRun(t)

	if err := os.MkdirAll(memory.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory.StateDir(), "watch-s1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--apply", manifestPath}) })
	if code != 2 || !strings.Contains(stderr, "live harness claims") {
		t.Errorf("not-quiesced apply: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(memory.StoreDir()); !os.IsNotExist(err) {
		t.Error("a refused apply must create no store")
	}
}

// Phase 1 safety: another worker holding the bank lock refuses the CLI apply with exit 2, never exit 1.
func TestMemoryMigrateApplyRefusesWhileBankLockBusy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := memory.GlobalDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bank, "conventions.md"), []byte("# C\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := manifestPathFromDryRun(t)

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(memory.BankLockPath(), 3*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	code, _, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--apply", manifestPath}) })
	if code != 2 || !strings.Contains(stderr, "memory store busy") {
		t.Errorf("bank-lock-busy apply: code=%d stderr=%q", code, stderr)
	}
}

// assertNoMigrationNotice runs one ordinary memory verb and checks it exits clean with no migration chatter.
func assertNoMigrationNotice(t *testing.T, verb []string) {
	t.Helper()
	code, _, stderr := captureOutput(t, func() int { return cmdMemory(verb) })
	if code != 0 {
		t.Errorf("memory %v: exit %d stderr=%q", verb, code, stderr)
	}
	if strings.Contains(stderr, "moved") || strings.Contains(stderr, "legacy") {
		t.Errorf("memory %v: unexpected migration notice in stderr=%q", verb, stderr)
	}
}

// Phase 1 safety (regression for the removed MigrateLegacy call site): an ordinary memory verb must
// never move, copy or inventory the legacy bank on its own — migration is only ever explicit.
func TestMemoryCommandsDoNotAutoMigrateLegacyBank(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := memory.GlobalDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(bank, "conventions.md")
	if err := os.WriteFile(sentinel, []byte("# C\n- untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, verb := range [][]string{{"path"}, {"search", "x"}, {"stats"}} {
		assertNoMigrationNotice(t, verb)
	}

	data, err := os.ReadFile(sentinel)
	if err != nil || !strings.Contains(string(data), "untouched") {
		t.Fatalf("legacy bank file moved or altered: err=%v data=%q", err, data)
	}
	manifests, _ := filepath.Glob(filepath.Join(memory.MigrateStateDir(), "manifest-*.json"))
	if len(manifests) != 0 {
		t.Errorf("an ordinary memory verb wrote a migration manifest: %v", manifests)
	}
	if _, err := os.Stat(memory.StoreDir()); !os.IsNotExist(err) {
		t.Error("an ordinary memory verb created the migration store")
	}
}

// Finding 3 surface: --supersede is the only CLI route to ApplySuperseding; it needs --apply, and with no other
// unfinished journal it behaves exactly like a plain apply.
func TestMemoryMigrateSupersedeFlag(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := memory.GlobalDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bank, "conventions.md"), []byte("# C\n- supersede\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := manifestPathFromDryRun(t)

	code, _, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--supersede"}) })
	if code != 1 || !strings.Contains(stderr, "--supersede needs --apply") {
		t.Errorf("--supersede alone: code=%d stderr=%q", code, stderr)
	}
	code, out, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--apply", manifestPath, "--supersede"}) })
	if code != 0 || !strings.Contains(out, "complete") {
		t.Fatalf("apply --supersede: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

// --format json is a dry-run contract: with --apply it must fail before touching the bank, never print plain text.
func TestMemoryMigrateApplyRejectsJSONFormat(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := memory.GlobalDir()
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bank, "conventions.md"), []byte("# C\n- json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := manifestPathFromDryRun(t)
	before, err := os.ReadFile(filepath.Join(bank, "conventions.md"))
	if err != nil {
		t.Fatal(err)
	}

	code, out, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--apply", manifestPath, "--format", "json"}) })
	if code != 1 || out != "" || !strings.Contains(stderr, "--format json only applies to a dry run") {
		t.Fatalf("apply --format json: code=%d out=%q stderr=%q", code, out, stderr)
	}
	after, err := os.ReadFile(filepath.Join(bank, "conventions.md"))
	if err != nil || string(after) != string(before) {
		t.Errorf("bank touched despite the rejected apply: err=%v after=%q", err, after)
	}
}
