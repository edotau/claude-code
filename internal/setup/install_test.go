package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentid"
	_ "github.com/edotau/claude-code/internal/codex"    // registers the codex engine the prod binary links
	_ "github.com/edotau/claude-code/internal/gemini"   // registers the gemini route engine the prod binary links
	_ "github.com/edotau/claude-code/internal/opencode" // registers the opencode engine the prod binary links
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/settings"
)

// TestInstallContinuesAfterSettingsFailure: installSettings failing (settings.json is a directory, so
// os.ReadFile returns an error that is not ErrNotExist) must not short-circuit Install — syncVendors and
// installGitHooks still run, and the error Install returns still names settings so the failure is legible.
// A corrupt state/vault.db (the seam the brief suggested) does not isolate this: providers.Load, called
// from config.Migrate before installSettings ever runs, opens the same vault.db and fails Migrate first,
// so Install returns at the earlier "migration incomplete" guard instead (already covered by
// TestInstallMigrationFailureKeepsOldBinary). Making settings.json itself unreadable is the seam that
// actually reaches installSettings without disturbing Migrate.
func TestInstallContinuesAfterSettingsFailure(t *testing.T) {
	hermetic(t)
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	file := settings.SettingsFile()
	if err := os.MkdirAll(file, 0o755); err != nil { // settings.json as a directory: os.ReadFile fails, not ErrNotExist
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := install(&out, false, true)
	if err == nil {
		t.Fatal("Install with an unreadable settings.json returned nil error")
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("Install err = %v, want it to name settings", err)
	}
	if !strings.Contains(out.String(), "settings skip") {
		t.Errorf("output missing the settings-skip report:\n%s", &out)
	}
	if !strings.Contains(out.String(), "agents   sync") {
		t.Errorf("syncVendors did not run after the settings failure:\n%s", &out)
	}
	if !strings.Contains(out.String(), "tmux") {
		t.Errorf("installGitHooks/earlier steps did not run either:\n%s", &out)
	}
}

// compactStepPrefix classifies one line of installCompact's default output by step; an unrecognized
// line fails immediately, so a renamed/new step's line must be taught here too.
func compactStepPrefix(t *testing.T, line string) string {
	t.Helper()
	switch {
	case strings.HasPrefix(line, "settings (windows)"):
		return "settings (windows)"
	case strings.HasPrefix(line, "install"):
		return "install"
	case strings.HasPrefix(line, "links"):
		return "links"
	case strings.HasPrefix(line, "settings"):
		return "settings"
	case strings.HasPrefix(line, "agents"):
		return "agents"
	case strings.HasPrefix(line, "extras"):
		return "extras"
	default:
		t.Fatalf("unrecognized line in default install output: %q", line)
		return ""
	}
}

// shortTempDir is a t.TempDir() alternative that doesn't embed the (often long) test name, so a
// path built under it stays representative of a real ~/.claude install for an 80-column check.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/private/tmp" // macOS $TMPDIR is /var/folders/…/T, itself too long for the 80-column check
	}
	dir, err := os.MkdirTemp(base, "ciq")
	if err != nil && base != "" {
		dir, err = os.MkdirTemp("", "ciq")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// setupCompactFixture wires a hermetic install where every name in linked resolves on PATH, so
// installCompact's "vendors" line folds them all into "linked"/"kept" instead of printing one of
// them in full. The config dir and HOME are re-rooted under a short path (hermetic's own t.TempDir()
// embeds the test name, which makes every path artificially long for the 80-column check) and real
// git stays reachable after the vendor fixtures (installGitHooks/history need it; it's absent from a
// bare vendor-only PATH). The host reads as non-WSL with HARNESS_WINDOWS_PROFILE empty, so no "settings (windows)"
// line prints on any machine unless a test sets the env.
func setupCompactFixture(t *testing.T, linked []string) {
	t.Helper()
	hermetic(t)
	root := shortTempDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	// hermetic() already redirected userBinDir/tmuxConfPaths, but at its own (long, test-name-based)
	// root; re-point them at the short root so their lines stay realistic-width too.
	oldUserBin := userBinDir
	userBinDir = func() string { return filepath.Join(root, "localbin") }
	t.Cleanup(func() { userBinDir = oldUserBin })
	oldTmux := tmuxConfPaths
	tmuxConfPaths = func() []string {
		return []string{filepath.Join(root, ".tmux.conf"), filepath.Join(root, "xdg", "tmux.conf")}
	}
	t.Cleanup(func() { tmuxConfPaths = oldTmux })
	self := filepath.Join(shortTempDir(t), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	var dirs []string
	for _, name := range linked {
		vendor := agentid.Binary(name)
		source := pathstest.FakeVendorOutsideConfig(t, vendor, "")
		dirs = append(dirs, filepath.Dir(source))
	}
	// Only git rides in from the real PATH (installGitHooks/history need it); every vendor not in
	// linked must resolve to nothing, not fall back to whatever this box happens to have installed.
	if p, err := exec.LookPath("git"); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	// userBinDir on PATH too, same as a real shell profile — otherwise linkOnPath's "not on PATH"
	// warning fires on every run here and this wouldn't be an all-ok fixture anymore.
	dirs = append(dirs, filepath.Join(root, "localbin"))
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
	noWindowsProfile(t)
}

// noWindowsProfile makes the host read as non-WSL with no Windows profile, so no "settings (windows)" step runs.
func noWindowsProfile(t *testing.T) {
	t.Helper()
	orig := onWSL
	onWSL = func() bool { return false } // never shell out to cmd.exe; a test opts into the windows line via the env
	t.Cleanup(func() { onWSL = orig })
	t.Setenv(windowsProfileEnv, "")
}

// TestInstallCompactDefaultOutputHasExactlyTheStepLines: an all-ok real run prints exactly one line
// per step (install, vendors, settings, settings (windows) only when applicable, agents, extras) —
// never a per-vendor or per-sub-step line, since nothing here failed or needed a full report.
func TestInstallCompactDefaultOutputHasExactlyTheStepLines(t *testing.T) {
	for _, windowsProfile := range []bool{false, true} {
		t.Run(fmt.Sprintf("windows profile %v", windowsProfile), func(t *testing.T) {
			setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini"})
			if windowsProfile { // a resolved Windows profile (WSL) adds the settings (windows) step
				t.Setenv(windowsProfileEnv, shortTempDir(t))
			}
			var out bytes.Buffer
			if err := install(&out, false, false); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, l := range splitNonEmpty(out.String()) {
				got = append(got, compactStepPrefix(t, l))
			}
			want := []string{"install", "links", "settings"}
			if windowsProfile {
				want = append(want, "settings (windows)")
			}
			want = append(want, "agents", "extras")
			if !slices.Equal(got, want) {
				t.Errorf("default install steps = %v, want %v\nfull output:\n%s", got, want, &out)
			}
		})
	}
}

// TestInstallCompactDefaultOutputLinesFitEightyColumns: the same all-ok fixture as above, checked for
// width — a compact line is meant to read on one terminal row.
func TestInstallCompactDefaultOutputLinesFitEightyColumns(t *testing.T) {
	setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini"})
	var out bytes.Buffer
	if err := install(&out, false, false); err != nil {
		t.Fatal(err)
	}
	for _, l := range splitNonEmpty(out.String()) {
		if len(l) > 80 {
			t.Errorf("default output line over 80 chars (%d): %q", len(l), l)
		}
	}
}

// TestInstallCompactDefaultModePrintsFailingVendorAndSettingsSkipInFull: a vendor installCompact
// can't link (copilot, left off PATH) and a settings.json read failure both still print their full
// detail line, even though the surrounding run is otherwise compact.
func TestInstallCompactDefaultModePrintsFailingVendorAndSettingsSkipInFull(t *testing.T) {
	setupCompactFixture(t, []string{"claude", "codex", "opencode", "gemini"}) // copilot left unlinked
	file := settings.SettingsFile()
	if err := os.MkdirAll(file, 0o755); err != nil { // settings.json as a directory: forces its read to fail
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := install(&out, false, false)
	if err == nil {
		t.Fatal("Install with an unreadable settings.json returned nil error")
	}
	if !strings.Contains(out.String(), "settings skip") {
		t.Errorf("default output missing the settings-skip line in full:\n%s", &out)
	}
	if !strings.Contains(out.String(), "copilot not installed") {
		t.Errorf("default output missing the unlinked vendor's full line:\n%s", &out)
	}
}

// historySnapshotHash normalizes the pre-install snapshot's commit hash, which is unique per git
// commit (tree/time/author) and so differs between two otherwise-identical fresh fixtures.
var historySnapshotHash = regexp.MustCompile(`history  [0-9a-f]+ \(pre-install snapshot\)`)

// runVerboseFixture sets up one hermetic, vendor-free install (so every vendor reports "missing"
// identically in both runs) and calls fn, returning its output plus the config-dir root and the
// self-binary path so the caller can normalize the two fixtures' distinct temp-dir names away.
func runVerboseFixture(t *testing.T, fn func(io.Writer) error) (output, root, self string) {
	t.Helper()
	hermetic(t)
	root = os.Getenv("CLAUDE_CONFIG_DIR")
	self = filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	t.Setenv("PATH", t.TempDir()) // an empty dir: every vendor resolves to "missing", deterministically
	noWindowsProfile(t)           // windows settings off in both fixtures alike
	var out bytes.Buffer
	if err := fn(&out); err != nil {
		t.Fatal(err)
	}
	return out.String(), root, self
}

func normalizeVerboseOutput(out, root, self string) string {
	out = historySnapshotHash.ReplaceAllString(out, "history  <hash> (pre-install snapshot)")
	out = strings.ReplaceAll(out, self, "$SELF")
	out = strings.ReplaceAll(out, root, "$ROOT")
	return out
}

// TestInstallVerboseFlagMatchesInstallVerboseDirectly: install(w, false, true) must route to the same
// installVerbose pipeline called directly — the --verbose/--dry-run contract this refactor promised
// to keep byte-identical, checked here without a stored golden (none existed before this change).
func TestInstallVerboseFlagMatchesInstallVerboseDirectly(t *testing.T) {
	outA, rootA, selfA := runVerboseFixture(t, func(w io.Writer) error { return install(w, false, true) })
	outB, rootB, selfB := runVerboseFixture(t, func(w io.Writer) error { return installVerbose(w, false) })
	normA := normalizeVerboseOutput(outA, rootA, selfA)
	normB := normalizeVerboseOutput(outB, rootB, selfB)
	if normA != normB {
		t.Errorf("Install(verbose=true) diverges from installVerbose called directly:\nA:\n%s\nB:\n%s", normA, normB)
	}
}

// The "still ran" settings error must print only after the steps it claims ran, in both output modes.
func TestInstallSettingsSkipPrintsAfterTheStepsItClaimsRan(t *testing.T) {
	for _, verbose := range []bool{true, false} {
		name := "compact"
		if verbose {
			name = "verbose"
		}
		t.Run(name, func(t *testing.T) {
			assertSettingsSkipOrder(t, verbose)
		})
	}
}

func assertSettingsSkipOrder(t *testing.T, verbose bool) {
	t.Helper()
	setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini"})
	if err := os.MkdirAll(settings.SettingsFile(), 0o755); err != nil { // settings.json as a directory: read fails
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := install(&out, false, verbose); err == nil {
		t.Fatal("Install with an unreadable settings.json returned nil error")
	}
	s := out.String()
	skip := strings.Index(s, "still ran")
	agents := strings.Index(s, "agents")
	if skip < 0 || agents < 0 {
		t.Fatalf("missing skip or agents line:\n%s", s)
	}
	if skip < agents {
		t.Errorf("settings skip printed before the agents step:\n%s", s)
	}
	if verbose {
		if g := strings.Index(s, "tmux"); g < 0 || skip < g {
			t.Errorf("settings skip printed before the git-hooks/extras steps:\n%s", s)
		}
	}
}

// Install must re-render agent configs so a stale harness hook in codex's hooks.json is purged.
func TestInstallRendersAgentConfigs(t *testing.T) {
	hermetic(t)
	home := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	hooksFile := filepath.Join(home, ".codex", "hooks.json")
	stale := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command",` +
		`"command":"HARNESS_HOOK_VENDOR=codex $HOME/.claude/bin/claude-code hook safety-scan"}]}]}}`
	if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksFile, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	got := agentConfigsSummary(&out)
	if !strings.Contains(got, "configs rendered:") || !strings.Contains(got, "codex") {
		t.Fatalf("summary = %q (warnings: %s)", got, out.String())
	}
	b, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "HARNESS_HOOK_VENDOR=codex $HOME") {
		t.Errorf("stale entry survived:\n%s", b)
	}
	if again := agentConfigsSummary(&out); again != "configs unchanged" {
		t.Errorf("second run = %q, want unchanged", again)
	}
}
