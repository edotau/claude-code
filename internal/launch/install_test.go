package launch

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini" // registers the gemini route engine the prod binary links
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	hermetic(t, "")
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	file := SettingsFile()
	if err := os.MkdirAll(file, 0o755); err != nil { // settings.json as a directory: os.ReadFile fails, not ErrNotExist
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Install(&out, false, true)
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

// A nested dir that holds only Claude Code's own worktrees is fine; other Keep reasons still ask for a move.
func TestNestedDetailWorktrees(t *testing.T) {
	only := clean.Nested{Dir: "/x/.claude", Files: []string{"worktrees"}, Keep: []string{clean.KeepWorktrees}}
	if !only.OnlyWorktrees() || !strings.Contains(nestedDetail(only), "holds only linked worktrees") {
		t.Fatalf("only-worktrees nested dir: %q", nestedDetail(only))
	}
	mixed := clean.Nested{Dir: "/x/.claude", Files: []string{"worktrees", "settings.json"}, Keep: []string{"settings.json:model", clean.KeepWorktrees}}
	if d := nestedDetail(mixed); mixed.OnlyWorktrees() || !strings.Contains(d, "move settings.json:model into") || strings.Contains(d, "worktrees") {
		t.Fatalf("mixed nested dir: %q", d)
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
// bare vendor-only PATH). HARNESS_WINDOWS_PROFILE is pointed at a resolvable dir when the host is
// really WSL (so the "settings (windows)" line is deterministic either way), empty otherwise.
func setupCompactFixture(t *testing.T, linked []string) (wantWindowsLine bool) {
	t.Helper()
	hermetic(t, "")
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
		vendor := paths.Vendors[name]
		source := vendorPATHFixture(t, vendor, "")
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
	if proc.OnWSL() {
		t.Setenv(windowsProfileEnv, shortTempDir(t))
		return true
	}
	t.Setenv(windowsProfileEnv, "")
	return false
}

// TestInstallCompactDefaultOutputHasExactlyTheStepLines: an all-ok real run prints exactly one line
// per step (install, vendors, settings, settings (windows) only when applicable, agents, extras) —
// never a per-vendor or per-sub-step line, since nothing here failed or needed a full report.
func TestInstallCompactDefaultOutputHasExactlyTheStepLines(t *testing.T) {
	wantWindows := setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini"})
	var out bytes.Buffer
	if err := Install(&out, false, false); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range splitNonEmpty(out.String()) {
		got = append(got, compactStepPrefix(t, l))
	}
	want := []string{"install", "links", "settings"}
	if wantWindows {
		want = append(want, "settings (windows)")
	}
	want = append(want, "agents", "extras")
	if !slices.Equal(got, want) {
		t.Errorf("default install steps = %v, want %v\nfull output:\n%s", got, want, &out)
	}
}

// TestInstallCompactDefaultOutputLinesFitEightyColumns: the same all-ok fixture as above, checked for
// width — a compact line is meant to read on one terminal row.
func TestInstallCompactDefaultOutputLinesFitEightyColumns(t *testing.T) {
	setupCompactFixture(t, []string{"claude", "codex", "copilot", "opencode", "gemini"})
	var out bytes.Buffer
	if err := Install(&out, false, false); err != nil {
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
	file := SettingsFile()
	if err := os.MkdirAll(file, 0o755); err != nil { // settings.json as a directory: forces its read to fail
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Install(&out, false, false)
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
	hermetic(t, "")
	root = os.Getenv("CLAUDE_CONFIG_DIR")
	self = filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	t.Setenv("PATH", t.TempDir())   // an empty dir: every vendor resolves to "missing", deterministically
	t.Setenv(windowsProfileEnv, "") // windows settings off in both fixtures alike
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

// TestInstallVerboseFlagMatchesInstallVerboseDirectly: Install(w, false, true) must route to the same
// installVerbose pipeline called directly — the --verbose/--dry-run contract this refactor promised
// to keep byte-identical, checked here without a stored golden (none existed before this change).
func TestInstallVerboseFlagMatchesInstallVerboseDirectly(t *testing.T) {
	outA, rootA, selfA := runVerboseFixture(t, func(w io.Writer) error { return Install(w, false, true) })
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
	if err := os.MkdirAll(SettingsFile(), 0o755); err != nil { // settings.json as a directory: read fails
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(&out, false, verbose); err == nil {
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

// seed writes a file under state/harness and returns its path.
func seedHarness(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(paths.StateDir(), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFormatsDoctorAndInstallStepSpan(t *testing.T) {
	hermetic(t, "")
	quota := seedHarness(t, "quota.json", `{"acct":{"until":"2099-01-01T00:00:00Z"}}`)
	seedHarness(t, "agentversion.json", `{"claude":"1.0.0"}`) // unversioned cache: a miss, never stale

	formats := findCheck(formatsDoctorChecks(), "formats")
	if formats == nil || !formats.OK || strings.Contains(formats.Detail, "stale") { // quota stays a bare map: an old daemon reads it
		t.Fatalf("formats row = %+v, want current", formats)
	}
	before, _ := os.ReadFile(quota)
	var out bytes.Buffer
	installFormats(&out, false)
	if after, _ := os.ReadFile(quota); !bytes.Equal(before, after) {
		t.Fatalf("install rewrote quota.json: %s", after)
	}
	if _, err := os.Stat(schemas.LedgerPath()); err == nil {
		t.Error("install wrote the ledger for a bare-map quota.json")
	}
}

// doctorScratch isolates CLAUDE_CONFIG_DIR, every credential, and opencode's own auth.json/opencode.jsonc
// (XDG_DATA_HOME/XDG_CONFIG_HOME) so DoctorChecks never reads real machine state.
func doctorScratch(t *testing.T) (xdgData string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir()) // no gh/copilot CLI reachable
	xdgData = filepath.Join(dir, "xdg-data")
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("HOME", dir) // never read the host's ~/.config/opencode or ~/.local/share/opencode
	for _, k := range []string{"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL",
		"CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN",
		"COPILOT_MODEL", "COPILOT_REASONING_EFFORT", "COPILOT_CONTEXT_TIER",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
	return xdgData
}

// workableCopilotPin pins a github profile whose login is user (a scratch hosts.yml) with 10% of its credits used.
func workableCopilotPin(t *testing.T, user string) {
	t.Helper()
	dir := t.TempDir()
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"work":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","login":"`+user+`","auth":{"type":"cli","login":"copilot"}}}}}`)
	vaulttest.SetRecord(t, "copilot", "profile", "work")
	prev := copilot.CopilotUsedPct
	copilot.CopilotUsedPct = func(context.Context, string) (float64, bool) { return 10, true }
	t.Cleanup(func() { copilot.CopilotUsedPct = prev })
}

// setPins seeds vault pins (pins never read the process env).
func findCheck(checks []Check, name string) *Check {
	for i := range checks {
		if checks[i].Name == name {
			return &checks[i]
		}
	}
	return nil
}

// Without opencode's own github-copilot login, the doctor row names the session provider, never a
// model/small_model pair (that's the github-copilot route's own shape).
func TestDoctorOpenCodeRouteWithoutCopilotLogin(t *testing.T) {
	doctorScratch(t)
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in DoctorChecks output`)
	}
	if strings.Contains(c.Detail, "small_model=") {
		t.Errorf("detail = %q, want no small_model on the non-copilot route", c.Detail)
	}
	if !strings.Contains(c.Detail, "opencode_login=false") {
		t.Errorf("detail = %q, want opencode_login=false", c.Detail)
	}
}

// With opencode's own github-copilot login present, the doctor row names provider=personal and
// both model/small_model: the registry now pins both aliases to concrete ids (no "auto" default to
// swap for OpencodeFallback any more — that swap path is covered directly in internal/agent).
func TestDoctorOpenCodeRouteWithCopilotLogin(t *testing.T) {
	xdgData := doctorScratch(t)
	// github-copilot's registry defaults are "auto" (Copilot's own router); pin real ids here so this
	// test still proves alias ids pass through unchanged, distinct from the auto -> OpencodeFallback swap.
	vaulttest.SetRecord(t, "claude", "aliases.sonnet", "gpt-5.3-codex")
	vaulttest.SetRecord(t, "claude", "aliases.haiku", "gpt-5.4-mini")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "provider=personal") {
		t.Errorf("detail = %q, want provider=personal", c.Detail)
	}
	if !strings.Contains(c.Detail, "opencode_login=true") {
		t.Errorf("detail = %q, want opencode_login=true", c.Detail)
	}
	if !strings.Contains(c.Detail, "model=github-copilot/gpt-5.3-codex") {
		t.Errorf("detail = %q, want the sonnet alias's own id, unchanged", c.Detail)
	}
	if !strings.Contains(c.Detail, "small_model=github-copilot/gpt-5.4-mini") {
		t.Errorf("detail = %q, want the haiku alias's own id, unchanged", c.Detail)
	}
}

// On the copilot CLI's own-login dispatch, the doctor row names COPILOT_MODEL's resolved source and the
// effort/context pins: unpinned, source is the github-copilot default and both pins read "default".
func TestDoctorCopilotRouteSourceEffortContextUnpinned(t *testing.T) {
	doctorScratch(t)
	workableCopilotPin(t, "alice")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "source=github-copilot default") {
		t.Errorf("detail = %q, want source=github-copilot default", c.Detail)
	}
	if !strings.Contains(c.Detail, "effort=default") || !strings.Contains(c.Detail, "context=default") {
		t.Errorf("detail = %q, want effort=default context=default", c.Detail)
	}
}

// With COPILOT_MODEL pinned and the effort/context pins set, the doctor row names the pin source
// and the pinned values, not the github-copilot provider default.
func TestDoctorCopilotRouteSourceEffortContextPinned(t *testing.T) {
	doctorScratch(t)
	workableCopilotPin(t, "alice")
	vaulttest.SetRecord(t, "copilot", "model", "claude-opus-5.5")
	vaulttest.SetRecord(t, "copilot", "effort", "high")
	vaulttest.SetRecord(t, "copilot", "context", "long_context")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in DoctorChecks output`)
	}
	if !strings.Contains(c.Detail, "model=claude-opus-5.5") {
		t.Errorf("detail = %q, want model=claude-opus-5.5", c.Detail)
	}
	if !strings.Contains(c.Detail, "source=pin") {
		t.Errorf("detail = %q, want source=pin", c.Detail)
	}
	if !strings.Contains(c.Detail, "effort=high") || !strings.Contains(c.Detail, "context=long_context") {
		t.Errorf("detail = %q, want effort=high context=long_context", c.Detail)
	}
}

// A stale static key in opencode's own auth.json for a provider the vault now covers fires the
// soft "opencode auth.json" check, naming the integration id and the logout command, never a value.
func TestDoctorOpenCodeAuthJSONStaleKeyFires(t *testing.T) {
	xdgData := doctorScratch(t)
	t.Setenv("NVIDIA_API_KEY", "nvapi-doctorFAKEkey1234567890")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"nvidia":{"type":"api","key":"nvapi-realFAKEsecretvalue"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in DoctorChecks output`)
	}
	if c.OK {
		t.Errorf("OK = true, want false: a stale vault-covered key must fail the soft check")
	}
	if c.Hard {
		t.Error("the check must be soft, never hard")
	}
	if !strings.Contains(c.Detail, "nvidia key") || !strings.Contains(c.Detail, "opencode auth logout nvidia") {
		t.Errorf("detail = %q, want it to name nvidia and the logout command", c.Detail)
	}
	if strings.Contains(c.Detail, "nvapi-realFAKEsecretvalue") {
		t.Fatalf("detail leaked the stored credential value: %q", c.Detail)
	}
}

// No stale key (auth.json absent, or the provider has no vault credential) reports clean.
func TestDoctorOpenCodeAuthJSONCleanWhenNoStaleKey(t *testing.T) {
	doctorScratch(t)
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in DoctorChecks output`)
	}
	if !c.OK || c.Detail != "clean" {
		t.Errorf("check = %+v, want OK=true Detail=clean", c)
	}
}

// An oauth connection (not a static "key" method) never counts as stale, even for a vault-covered provider.
func TestDoctorOpenCodeAuthJSONIgnoresOAuthConnections(t *testing.T) {
	xdgData := doctorScratch(t)
	t.Setenv("NVIDIA_API_KEY", "nvapi-doctorFAKEkey1234567890")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"nvidia":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := DoctorChecks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in DoctorChecks output`)
	}
	if !c.OK || c.Detail != "clean" {
		t.Errorf("check = %+v, want OK=true Detail=clean for an oauth-only connection", c)
	}
}

// The doctor row never prints a secret: GH_TOKEN's value must not surface, only the env var's presence.
func TestDoctorOpenCodeRouteNamesNoSecret(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-gh-token-value")
	checks, _ := DoctorChecks()
	for _, c := range checks {
		if strings.Contains(c.Detail, "super-secret-gh-token-value") {
			t.Fatalf("check %q leaked a credential: %q", c.Name, c.Detail)
		}
	}
}

// A key-auth built-in nothing routes to (not session, default, fallback or pinned) is a soft note, not ✗;
// pinning it for an agent makes the missing key a real ✗ again.
func TestDoctorUnusedProviderMissingKeyIsSoft(t *testing.T) {
	doctorScratch(t)
	checks, _ := DoctorChecks()
	c := findCheck(checks, "credential openai")
	if c == nil || !c.OK || !strings.Contains(c.Detail, "unused") {
		t.Fatalf("unused openai row = %+v, want OK with an unused note", c)
	}
	vaulttest.SetRecord(t, "codex", "profile", "openai")
	checks, _ = DoctorChecks()
	if c := findCheck(checks, "credential openai"); c == nil || c.OK {
		t.Fatalf("pinned openai row = %+v, want ✗ for the missing key", c)
	}
}

// altAuthScratch isolates the machine state altAuth reads: CLAUDE_CONFIG_DIR (keyring/secrets.env),
// HOME (the gemini agy login marker, and claude.ClaudeLoginProbe has nowhere real to run), and a PATH with
// neither `claude` nor `security`/`secret-tool` on it, so the real Keychain and a real claude login
// can never answer for this test by accident.
func altAuthScratch(t *testing.T) (home string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir())
	home = filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(providers.OAuthTokenEnv, "")
	return home
}

func TestAltAuthAnthropicHeadlessToken(t *testing.T) {
	altAuthScratch(t)
	t.Setenv(providers.OAuthTokenEnv, "sk-ant-oat-test")
	if got := altAuth(nil, "anthropic"); got != "claude uses the OAuth token instead" {
		t.Errorf("altAuth(anthropic) with a headless token = %q", got)
	}
}

func TestAltAuthAnthropicNone(t *testing.T) {
	altAuthScratch(t) // no headless token, no claude binary to report a login
	if got := altAuth(nil, "anthropic"); got != "" {
		t.Errorf("altAuth(anthropic) with neither = %q, want \"\"", got)
	}
}

// gemini has no altAuth case: GeminiLoginPresent (not altAuth) now reports the agy Google login.
func TestAltAuthGemini(t *testing.T) {
	home := altAuthScratch(t)
	if got := altAuth(nil, "gemini"); got != "" {
		t.Errorf("altAuth(gemini) without the agy marker = %q, want \"\"", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := altAuth(nil, "gemini"); got != "" {
		t.Errorf("altAuth(gemini) with the agy marker = %q, want \"\" (altAuth has no gemini case)", got)
	}
}

func TestAltAuthUnknownProvider(t *testing.T) {
	altAuthScratch(t)
	if got := altAuth(nil, "openai"); got != "" {
		t.Errorf("altAuth(openai) = %q, want \"\" (no alt auth path for this provider)", got)
	}
}

// RouteDetail is the per-vendor shim `vendoragent setup` calls instead of the whole doctor sweep; its detail must
// equal the "<vendor> route" row DoctorChecks prints, for every vendor whose engine is registered.
func TestRouteDetailEqualsDoctorRouteRow(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "ghp_doctorFAKEtoken1234567890")
	checks, _ := DoctorChecks()
	for _, vendor := range []string{"codex", "gemini", "opencode", "copilot"} {
		detail, ok := agent.RouteDetail(vendor)
		c := findCheck(checks, vendor+" route")
		switch {
		case !ok:
			t.Errorf("RouteDetail(%q) ok = false, want true", vendor)
		case c == nil:
			t.Errorf("no %q row in DoctorChecks", vendor+" route")
		case detail == "" || detail != c.Detail:
			t.Errorf("RouteDetail(%q) = %q, doctor row = %q", vendor, detail, c.Detail)
		}
	}
}

// An unknown vendor reports ok=false, never a stale/zero detail string.
func TestRouteDetailUnknownVendor(t *testing.T) {
	doctorScratch(t)
	if detail, ok := agent.RouteDetail("not-a-vendor"); ok || detail != "" {
		t.Errorf("RouteDetail(unknown) = (%q, %v), want (\"\", false)", detail, ok)
	}
}

// TestPrintChecksGoldenBytes pins printChecks' byte format (color off, the case for every test and
// any non-tty run): "<mark> <name>: <detail>\n", mark = ✓ when OK else ✗. Doctor and VaultDoctor both
// share this function now; a drift here changes what both commands print.
func TestPrintChecksGoldenBytes(t *testing.T) {
	checks := []Check{
		{Name: "sample", OK: true, Detail: "all good"},
		{Name: "sample2", OK: false, Detail: "broken: reason"},
	}
	var buf bytes.Buffer
	PrintChecks(&buf, checks)
	want := "✓ sample: all good\n✗ sample2: broken: reason\n"
	if got := buf.String(); got != want {
		t.Errorf("printChecks =\n%q\nwant\n%q", got, want)
	}
}

// Install must re-render agent configs so a stale harness hook in codex's hooks.json is purged.
func TestInstallRendersAgentConfigs(t *testing.T) {
	hermetic(t, "")
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
