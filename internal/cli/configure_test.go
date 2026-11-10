package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/ui/menu"
)

// TestConfigureHelpFitsEightyColumns: `configure -h` lines (usage + flag defaults) stay within 80 columns.
func TestConfigureHelpFitsEightyColumns(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"-h"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for l := range strings.SplitSeq(stderr, "\n") {
		if n := utf8.RuneCountInString(l); n > 80 {
			t.Errorf("help line is %d cols: %q", n, l)
		}
	}
}

func run(args []string) int { return cmdConfigure(args, func(string, string) int { return 0 }) }

// TestSpecArgsOfferSectionsThenProfiles: the first bare argument completes from the menu sections.
func TestSpecArgsOfferSectionsThenProfiles(t *testing.T) {
	want := append([]string{}, menu.SectionNames...)
	if got := configureSpec().Args[0](); !slices.Equal(got, want) {
		t.Errorf("sections = %v, want %v", got, want)
	}
}

func TestConfigureHelpShowsDoubleDashFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"-h"}) })
	if !strings.Contains(stderr, "--restore") {
		t.Errorf("help lacks --restore:\n%s", stderr)
	}
	if strings.Contains(stderr, "\n  -restore") {
		t.Errorf("help prints single-dash -restore:\n%s", stderr)
	}
}

// resetEnv isolates HOME and CLAUDE_CONFIG_DIR so --restore renders only into a scratch dir.
func resetEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	return dir
}

func TestConfigureRestoreRejectsBadCombos(t *testing.T) {
	resetEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"restore with page", []string{"--restore", "provider"}, "take no page"},
		{"revert with page", []string{"--revert", "agents"}, "take no page"},
		{"restore and revert", []string{"--restore", "--revert"}, "mutually exclusive"},
		{"revert with check", []string{"--revert", "--check"}, "--revert takes only"},
		{"revert with out", []string{"--revert", "--out", "x.json"}, "--revert takes only"},
		{"revert with agents", []string{"--revert", "--agents"}, "--revert takes only"},
		{"out without restore", []string{"--out", "x.json"}, "need --restore"},
		{"agents without restore", []string{"--agents"}, "need --restore"},
		{"restore with check", []string{"--restore", "--check"}, "takes no --check or --dry-run"},
		{"restore with dry-run", []string{"--restore", "--dry-run"}, "takes no --check or --dry-run"},
		{"out with agents", []string{"--restore", "--agents", "--out", "x.json"}, "never write one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q lacks %q", stderr, tc.want)
			}
		})
	}
}

// TestConfigureRestoreResetsSettingsAfterBackup: bare --restore copies settings.json aside, then writes the render.
func TestConfigureRestoreResetsSettingsAfterBackup(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"--restore"}) })
	if code != 0 {
		t.Fatalf("bare --restore exit %d: %s", code, stderr)
	}
	if got, _ := os.ReadFile(live); strings.Contains(string(got), "stale") || !strings.Contains(string(got), "--safety-scan") {
		t.Fatalf("settings.json is not the render:\n%s", got)
	}
	_, after, ok := strings.Cut(stdout, "(backup ")
	if !ok {
		t.Fatalf("no backup reported:\n%s", stdout)
	}
	bk, _, _ := strings.Cut(after, ")")
	if old, err := os.ReadFile(bk); err != nil || string(old) != `{"stale":true}` || !strings.HasPrefix(bk, live+".bak.") {
		t.Errorf("backup %q missing or wrong (err %v): %s", bk, err, old)
	}
}

// TestConfigureRestoreAgentsNeverTouchesSettings: --agents/--agent render only the agent configs.
func TestConfigureRestoreAgentsNeverTouchesSettings(t *testing.T) {
	dir := resetEnv(t)
	live := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(live, []byte(`{"model":"mine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--restore", "--agents", "--agent", "codex"}, {"--restore", "--agent", "codex"}} {
		if code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(args) }); code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, stderr)
		}
		baks, _ := filepath.Glob(live + ".bak.*")
		if got, _ := os.ReadFile(live); string(got) != `{"model":"mine"}` || len(baks) != 0 {
			t.Errorf("%v touched settings.json: %s (backups %v)", args, got, baks)
		}
	}
}

func TestConfigureRestoreRejectsUnknownAgent(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return run([]string{"--restore", "--agents", "--agent", "x"})
	})
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unknown --agent "x"`) {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestConfigureAllFlagGates(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		want     string
	}{
		{"old databricks --all is gone", []string{"databricks", "--all"}, 1, "--all needs the provider page"},
		{"agents --all", []string{"agents", "--all"}, 1, "--all needs the provider page"},
		{"unknown profile", []string{"provider", "--all", "no-such-row"}, 1, "unknown profile no-such-row"},
		{"--family removed", []string{"provider", "--all", "--family", "x"}, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != tc.wantCode || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d (want %d), stderr %q lacks %q", code, tc.wantCode, stderr, tc.want)
			}
		})
	}
}

func TestConfigureAllArgumentLimits(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"two profiles", []string{"provider", "--all", "a", "b"}, "at most one <profile> (got 2)"},
		{"with --list", []string{"provider", "--all", "--list"}, "cannot be combined with --list or --profile"},
		{"with --profile", []string{"provider", "--all", "--profile", "x"}, "cannot be combined with --list or --profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q lacks %q", code, stderr, tc.want)
			}
		})
	}
}

// TestConfigureAuthRejectedOutsideGithubPage: --auth is refused before the restore and list branches can drop it.
func TestConfigureAuthRejectedOutsideGithubPage(t *testing.T) {
	resetEnv(t)
	cases := []struct {
		name string
		args []string
	}{
		{"restore", []string{"--restore", "--auth", "follow"}},
		{"revert", []string{"--revert", "--auth", "follow"}},
		{"json list", []string{"provider", "--auth", "follow", "--format", "json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != 1 || !strings.Contains(stderr, "--auth needs the github page") {
				t.Errorf("exit %d, stderr %q; want 1 and the --auth refusal", code, stderr)
			}
		})
	}
}

// TestConfigureAuthRejectedWithRestore: on the github page --auth with --restore or --revert fails instead of being dropped.
func TestConfigureAuthRejectedWithRestore(t *testing.T) {
	resetEnv(t)
	for _, flagName := range []string{"--restore", "--revert"} {
		t.Run(flagName, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				return run([]string{"github", "--auth", "follow", flagName})
			})
			if code != 1 || !strings.Contains(stderr, "--auth cannot be combined with --restore or --revert") {
				t.Errorf("exit %d, stderr %q; want 1 and the combination refusal", code, stderr)
			}
		})
	}
}
