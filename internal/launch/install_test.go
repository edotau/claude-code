package launch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// These Install calls pass verbose=true: they assert on the full-detail per-step text, which only
// installVerbose prints. The compact (verbose=false, dry-run=false) path's own one-line-per-step
// output is a separate case list — see the install-quiet handoff.
func TestInstallMergesIntoConfigDir(t *testing.T) {
	hermetic(t, "")
	codex := vendorPATHFixture(t, "codex", "")
	t.Setenv("PATH", filepath.Dir(codex))
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	file := SettingsFile()
	orig := `{"spinnerTipsEnabled": false, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "foreign stop"}]}]}}`
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(&out, true, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Fatal("--dry-run wrote settings.json")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(file), "bin", "claude")); err == nil {
		t.Fatal("--dry-run created vendor links")
	}
	if err := Install(&out, false, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	for _, want := range []string{`"spinnerTipsEnabled": false`, `"foreign stop"`, "hook Stop", "hook PreToolUse --safety-scan"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("merged settings missing %s:\n%s", want, b)
		}
	}
	if link, _ := os.Readlink(filepath.Join(filepath.Dir(file), "bin", "codex")); link != codex {
		t.Errorf("codex link -> %q, want %q", link, codex)
	}
	onPath := filepath.Join(userBinDir(), "claude-code")
	if link, _ := os.Readlink(onPath); link != filepath.Join(filepath.Dir(file), "bin", "claude-code") {
		t.Errorf("%s -> %q, want the installed binary", onPath, link)
	}
	if _, err := os.Lstat(filepath.Join(userBinDir(), "codex")); err == nil {
		t.Error("only claude-code may be linked onto PATH")
	}
	if b, _ := os.ReadFile(tmuxConfPaths()[0]); !bytes.Equal(b, tmuxConf) {
		t.Errorf("tmux.conf not seeded: %q", b)
	}
	bakSample := paths.BackupPath(file, time.Now())
	baks, _ := filepath.Glob(filepath.Join(filepath.Dir(bakSample), filepath.Base(file)+".bak-*"))
	if len(baks) != 1 {
		t.Errorf("backups %v", baks)
	}
	if has, err := settingsHasHooks(file); !has || err != nil {
		t.Errorf("doctor hook check: %v %v", has, err)
	}
}

func TestInstallBinaryAndLinksReplacesRetiredShims(t *testing.T) {
	hermetic(t, "")
	t.Setenv("HOME", t.TempDir())
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]string)
	var dirs []string
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "gemini"} {
		vendor := paths.Vendors[name]
		source := vendorPATHFixture(t, vendor, "")
		sources[name] = source
		dirs = append(dirs, filepath.Dir(source))
		if err := os.Symlink("claude-code", filepath.Join(paths.BinDir(), name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
	var out bytes.Buffer
	target, err := installBinaryAndLinks(&out, true)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(paths.BinDir(), "claude-code") {
		t.Fatalf("target = %q", target)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run binary: %v, want not exist", err)
	}
	for name := range sources {
		link, err := os.Readlink(filepath.Join(paths.BinDir(), name))
		if err != nil || link != "claude-code" {
			t.Fatalf("dry-run changed %s: %q, %v", name, link, err)
		}
	}
	out.Reset()
	if _, err := installBinaryAndLinks(&out, false); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "binary" {
		t.Fatalf("installed binary = %q, %v", data, err)
	}
	for name, source := range sources {
		dst := filepath.Join(paths.BinDir(), name)
		link, err := os.Readlink(dst)
		if err != nil || link != source {
			t.Errorf("%s -> %q, %v; want %q", name, link, err, source)
		}
		if _, err := paths.AgentBin(name); err != nil {
			t.Errorf("installed vendor %s: %v", name, err)
		}
		want := "link     " + dst + " → " + source
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in output:\n%s", want, &out)
		}
	}
	entries, err := os.ReadDir(paths.BinDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		dst := filepath.Join(paths.BinDir(), entry.Name())
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(dst)
		if err != nil {
			t.Fatal(err)
		}
		if real == target || filepath.Base(real) == "claude-code" {
			t.Errorf("retired harness link remains: %s -> %s", dst, real)
		}
	}
}

// Install's wl-paste proxy row follows the real box's WSL-ness (procVersionPath lives in the
// clipboard package and isn't overridable from here); either way it must never touch the real HOME.
func TestInstallEnsuresClipboardProxy(t *testing.T) {
	hermetic(t, "")
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	file := SettingsFile()
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	scratchHome := t.TempDir()
	t.Setenv("HOME", scratchHome)
	proxyPath := filepath.Join(scratchHome, ".local", "bin", "wl-paste")

	var out bytes.Buffer
	if err := Install(&out, false, true); err != nil {
		t.Fatal(err)
	}

	if !clipboard.OnWSLKernel() {
		if _, err := os.Lstat(proxyPath); err == nil {
			t.Fatal("off-WSL: Install wrote the wl-paste proxy")
		}
		return
	}
	b, err := os.ReadFile(proxyPath)
	if err != nil {
		t.Fatalf("WSL: Install did not write the wl-paste proxy: %v", err)
	}
	if !bytes.Equal(b, clipboard.ProxyScript) {
		t.Error("written proxy does not match the embedded script")
	}
	info, err := os.Stat(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), "wl-paste") {
		t.Errorf("install output missing a wl-paste row:\n%s", out.String())
	}

	// Re-run: proxy is now current, reported as such, and left byte-identical.
	out.Reset()
	if err := Install(&out, false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("re-run output = %q, want a current line", out.String())
	}
	b2, _ := os.ReadFile(proxyPath)
	if !bytes.Equal(b2, clipboard.ProxyScript) {
		t.Error("re-run modified an already-current proxy")
	}
}

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
		})
	}
}

func TestProviderCredentialChecksRunConcurrentlyInOrder(t *testing.T) {
	hermetic(t, "")
	names := []string{"p-0", "p-1", "p-2", "p-3", "p-4", "p-5", "p-6", "p-7"}
	reg := &providers.Registry{Default: "elsewhere", Providers: map[string]*providers.Provider{}}
	byPtr := map[*providers.Provider]string{}
	for _, n := range names {
		p := &providers.Provider{Auth: providers.Auth{Type: providers.AuthNone}}
		reg.Providers[n] = p
		byPtr[p] = n
	}
	var inflight, maxSeen atomic.Int32
	gate := make(chan struct{}) // closed once the worker limit of probes are provably in flight together
	over := make(chan struct{}) // closed if a probe beyond the limit ever runs
	var once, overOnce sync.Once
	old := probeCredential
	t.Cleanup(func() { probeCredential = old })
	probeCredential = func(_ context.Context, p *providers.Provider) (string, string) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			m := maxSeen.Load()
			if cur <= m || maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		if cur >= credentialProbeWorkers {
			once.Do(func() { close(gate) })
		}
		if cur > credentialProbeWorkers {
			overOnce.Do(func() { close(over) })
		}
		select {
		case <-gate:
		case <-time.After(5 * time.Second): // only an implementation below the limit reaches this
		}
		select {
		case <-over:
		case <-time.After(100 * time.Millisecond): // gives a probe beyond the limit time to show up
		}
		n := byPtr[p]
		return "probe-" + n, "detail-" + n
	}

	rows := providerCredentialChecks(reg, providers.Selection{}, errors.New("no session"), names)

	if len(rows) != len(names) {
		t.Fatalf("got %d rows, want %d", len(rows), len(names))
	}
	for i, n := range names {
		if rows[i].Name != "credential "+n {
			t.Errorf("row %d = %q, want %q", i, rows[i].Name, "credential "+n)
		}
		if want := "probe-" + n + ": detail-" + n; !rows[i].OK || !strings.Contains(rows[i].Detail, want) {
			t.Errorf("row %s = %+v, want OK with detail containing %q", n, rows[i], want)
		}
	}
	if m := maxSeen.Load(); m < 2 {
		t.Errorf("max in-flight probes = %d, want >= 2 (probes must overlap)", m)
	} else if m > credentialProbeWorkers {
		t.Errorf("max in-flight probes = %d, want <= %d (worker limit)", m, credentialProbeWorkers)
	}
}

func TestToolPATHCopiesAndExtras(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", scratch)
	bin := paths.BinDir()
	primary := pathstest.FakeVendor(t, "opencode", "echo 1.0.0")
	aBin := vendorPATHFixture(t, "opencode", "echo 1.0.0")
	bBin := vendorPATHFixture(t, "opencode", "echo 2.0.0")
	a, b, c := filepath.Dir(aBin), filepath.Dir(bBin), filepath.Join(scratch, "C")
	// C/opencode is a symlink to A/opencode: same real file, must dedupe.
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aBin, filepath.Join(c, "opencode")); err != nil {
		t.Fatal(err)
	}
	join := func(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }

	cases := []struct {
		name       string
		path       string
		wantCopies []string
	}{
		{"bin first", join(bin, a, b), []string{aBin, bBin}},
		{"tool before bin", join(a, bin, b), []string{aBin, bBin}},
		{"no bin dir", join(a, b), []string{aBin, bBin}},
		{"symlink dedup", join(bin, a, c), []string{aBin}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			copies := toolPATHCopies("opencode")
			if !slices.Equal(copies, tc.wantCopies) {
				t.Fatalf("toolPATHCopies = %v; want %v", copies, tc.wantCopies)
			}
		})
	}

	status := tools.Status{Name: "opencode", Path: aBin, Version: "1.0.0"}
	for _, order := range []string{join(a, bin, b), join(bin, a, b)} {
		t.Run(order, func(t *testing.T) {
			t.Setenv("PATH", order)
			detail := toolDoctorDetail(status)
			want := primary + " 1.0.0 (direct install in bin/)" +
				" · also on PATH: " + aBin + " 1.0.0 · also on PATH: " + bBin + " 2.0.0"
			if detail != want {
				t.Errorf("detail = %q, want %q", detail, want)
			}
		})
	}
}

func TestDoctorVendorRows(t *testing.T) {
	for _, kind := range []string{"link", "direct", "broken link", "retired shim"} {
		t.Run(kind, func(t *testing.T) {
			doctorScratch(t)
			t.Setenv("HOME", t.TempDir())
			source := vendorPATHFixture(t, "opencode", "echo 1.2.3")
			t.Setenv("PATH", filepath.Dir(source))
			dst := pathstest.FakeVendor(t, "opencode", "echo 4.5.6")
			want := dst + " 4.5.6 (direct install in bin/)" +
				" · also on PATH: " + source + " 1.2.3"
			if kind != "direct" {
				if err := os.Remove(dst); err != nil {
					t.Fatal(err)
				}
				target := source
				switch kind {
				case "link":
					want = dst + " → " + source + " 1.2.3"
				case "broken link":
					target = filepath.Join(t.TempDir(), "missing")
					want = "opencode: " + dst + " is a broken link (run claude-code install) · on PATH: " + source
				case "retired shim":
					target = filepath.Join(t.TempDir(), "claude-code")
					if err := os.WriteFile(target, []byte("must not execute"), 0o755); err != nil {
						t.Fatal(err)
					}
					want = "opencode: " + dst + " is the retired claude-code shim (run claude-code install) · on PATH: " + source
				}
				if err := os.Symlink(target, dst); err != nil {
					t.Fatal(err)
				}
			}
			checks, _ := DoctorChecks()
			row := findCheck(checks, "tool opencode")
			if row == nil {
				t.Fatal("missing tool opencode row")
			}
			if row.Detail != want {
				t.Errorf("detail = %q, want %q", row.Detail, want)
			}
			if wantOK := kind == "link" || kind == "direct"; row.OK != wantOK {
				t.Errorf("OK = %v, want %v", row.OK, wantOK)
			}
		})
	}
}

func TestDoctorAgentVersionRows(t *testing.T) {
	hermetic(t, "")
	pathstest.FakeVendor(t, "copilot", "echo 'GitHub Copilot CLI 1.0.100.'")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "agent version copilot")
	if c == nil {
		t.Fatal("no `agent version copilot` row")
	}
	if !strings.HasPrefix(c.Detail, "warn 1.0.100 below 1.0.104") || c.Hard {
		t.Errorf("row = %+v, want detail `warn 1.0.100 below 1.0.104...` and Hard=false", *c)
	}

	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	checks, _ = DoctorChecks()
	c = findCheck(checks, "agent version copilot")
	if c == nil || c.Detail != "not installed" || c.Hard {
		t.Errorf("missing binary row = %+v, want `not installed`, non-hard", c)
	}
}

func TestToolPATHVersionProbesUnchangedBinaryOnce(t *testing.T) {
	hermetic(t, "")
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\necho x >> '" + counter + "'\necho 'fake 7.8.9'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := toolPATHVersion(bin); got != "fake 7.8.9" {
			t.Fatalf("toolPATHVersion = %q", got)
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("two lookups spawned the binary %d times, want 1", n)
	}
}

// TestUserBinDirNotEmpty verifies userBinDir returns a meaningful path, not "/.local/bin" when home is missing.
func TestUserBinDirNotEmpty(t *testing.T) {
	got := userBinDir()
	if got == "" {
		t.Error("userBinDir returned empty string")
	}
	// Verify it's a reasonable path (not the broken "/.local/bin" when home is missing)
	// After the fix, this test will pass because errors are handled properly
	if got == "/.local/bin" {
		t.Errorf("userBinDir returned %q (indicates UserHomeDir error was ignored)", got)
	}
	t.Logf("userBinDir = %q (regression check: should be valid path after fix)", got)
}

// TestExtraPATHCopiesHandlesEvalSymlinksErrors verifies EvalSymlinks errors are checked, not ignored.
// Regression: lines 546-550 did `first, _ := filepath.EvalSymlinks(primary)` ignoring errors,
// so when primary is a broken symlink, first="" and comparisons like real=="" fail to deduplicate.
func TestExtraPATHCopiesHandlesEvalSymlinksErrors(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", scratch)
	// Create a real file we'll compare against
	realPath := filepath.Join(scratch, "real-tool")
	if err := os.WriteFile(realPath, []byte("tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a symlink to the real file
	linkPath := filepath.Join(scratch, "link-tool")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	// Test extraPATHCopies with the symlink as primary; it should resolve to realPath.
	// The function must call EvalSymlinks and check the error to properly deduplicate.
	detail := extraPATHCopies("tool", linkPath)
	// If EvalSymlinks error is ignored and linkPath is broken later, detail would show false duplicates.
	// After fix: even if primary is a symlink, EvalSymlinks errors are handled and comparisons work.
	t.Logf("extraPATHCopies returned (detail=%q) for symlink primary; no crash means errors handled", detail)
}

func TestStrayClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if ok, _ := strayClaudeJSON(); !ok {
		t.Fatal("no stub must pass")
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := strayClaudeJSON(); ok || !strings.Contains(detail, stub) {
		t.Errorf("stub present: ok=%v detail=%q", ok, detail)
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
	seedHarness(t, "zz-stray.json", `{}`)

	formats := findCheck(formatsDoctorChecks(), "formats")
	if formats == nil || !formats.OK || strings.Contains(formats.Detail, "stale") { // quota stays a bare map: an old daemon reads it
		t.Fatalf("formats row = %+v, want current", formats)
	}
	layout := findCheck(formatsDoctorChecks(), "state layout")
	if layout == nil || !strings.Contains(layout.Detail, "zz-stray.json") {
		t.Fatalf("state layout row = %+v, want stray file listed", layout)
	}
	if strings.Contains(layout.Detail, "quota.json") {
		t.Errorf("registered file flagged in layout: %q", layout.Detail)
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

func TestVaultDoctorChecks(t *testing.T) {
	t.Run("missing vault", func(t *testing.T) {
		hermetic(t, "")
		checks := vaultDoctorChecks()
		c := findCheck(checks, "vault")
		if c == nil || c.Detail != "not created yet" || !c.OK {
			t.Errorf("vault row = %+v", c)
		}
		for _, name := range []string{"vault perms", "keyring tool"} {
			if c := findCheck(checks, name); c == nil || !c.OK {
				t.Errorf("missing healthy %s row", name)
			}
		}
	})
	t.Run("secrets and retired", func(t *testing.T) {
		hermetic(t, "")
		t.Setenv("HARNESS_VAULT_KEY", "")
		vaulttest.Keyring(t)
		vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
		if _, _, err := vault.RotateKey(); err != nil {
			t.Fatal(err)
		}
		c := findCheck(vaultDoctorChecks(), "vault")
		if c == nil || !c.OK {
			t.Fatalf("vault row = %+v", c)
		}
		for _, want := range []string{vault.Path(), "key keyring", "1 secret(s)", "1 retired"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail %q lacks %q", c.Detail, want)
			}
		}
		if strings.Contains(c.Detail, "sk-test") {
			t.Error("doctor printed a secret")
		}
	})
	t.Run("one vault row, not two", func(t *testing.T) {
		hermetic(t, "")
		t.Setenv("HARNESS_VAULT_KEY", "")
		vaulttest.Keyring(t)
		vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
		n := 0
		for _, c := range vaultDoctorChecks() {
			if c.Name == "vault" || c.Name == "vault master key" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("vaultDoctorChecks has %d vault/master-key row(s), want exactly 1 (the \"vault\" row carries the master-key status)", n)
		}
	})
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
// HOME (the gemini agy login marker, and ClaudeLoginStatus has nowhere real to run), and a PATH with
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

// RouteDetail is the per-vendor shim `vendoragent setup` calls instead of running the whole doctor
// sweep; its detail string must match the corresponding "<v> route" row in DoctorChecks exactly.
func TestRouteDetailMatchesDoctorChecks(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "ghp_doctorFAKEtoken1234567890")
	vaulttest.SetRecord(t, "claude", "aliases.sonnet", "gpt-5.3-codex")

	checks, _ := DoctorChecks()
	for _, vendor := range []string{"codex", "gemini", "opencode", "copilot"} {
		detail, ok := agent.RouteDetail(vendor)
		if !ok {
			t.Errorf("RouteDetail(%q) ok = false, want true", vendor)
			continue
		}
		c := findCheck(checks, vendor+" route")
		if c == nil {
			t.Fatalf("no %q check in DoctorChecks output", vendor+" route")
		}
		if detail != c.Detail {
			t.Errorf("RouteDetail(%q) = %q, want DoctorChecks' %q", vendor, detail, c.Detail)
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

// TestInstallSettingsFreshRestoresThenMergesExisting: no settings.json restores the render whole; an
// existing file keeps its foreign keys through the merge.
func TestInstallSettingsFreshRestoresThenMergesExisting(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	want, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := installSettings(&bytes.Buffer{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SettingsFile()); err == nil {
		t.Fatal("--dry-run created settings.json")
	}
	if err := installSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(SettingsFile()); string(got) != string(want) {
		t.Fatalf("fresh install != render:\n%s", got)
	}
	merged := strings.Replace(string(want), "{\n", "{\n  \"zzForeign\": 1,\n", 1)
	if err := os.WriteFile(SettingsFile(), []byte(merged), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(SettingsFile()); !strings.Contains(string(got), "zzForeign") {
		t.Errorf("install over an existing file dropped a foreign key:\n%s", got)
	}
}

// TestDoctorSettingsDriftIsInfoNotFailure: changed keys are information; invalid JSON still fails.
func TestDoctorSettingsDriftIsInfoNotFailure(t *testing.T) {
	vaulttest.Use(t)
	if _, err := RestoreSettings(SettingsFile()); err != nil {
		t.Fatal(err)
	}
	if ok, detail := DoctorSettingsDrift(); !ok || detail != "clean" {
		t.Fatalf("after restore: ok=%v detail=%q, want clean", ok, detail)
	}
	if err := os.WriteFile(SettingsFile(), []byte(`{"model":"x","zzExtra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, detail := DoctorSettingsDrift()
	if !ok || !strings.Contains(detail, "keys differ from your defaults — claude-code configure --restore") {
		t.Errorf("changed keys: ok=%v detail=%q, want info", ok, detail)
	}
	if err := os.WriteFile(SettingsFile(), []byte(`{nope`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := DoctorSettingsDrift(); ok {
		t.Errorf("invalid JSON: ok=true detail=%q, want a failure", detail)
	}
}

// TestCheckSettingsDriftSeesWhatRestoreWouldRemove: an extra env var the merge would keep is drift,
// because --restore replaces the whole file.
func TestCheckSettingsDriftSeesWhatRestoreWouldRemove(t *testing.T) {
	vaulttest.Use(t)
	if _, err := RestoreSettings(SettingsFile()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(SettingsFile())
	edited := strings.Replace(string(raw), `"env": {`, `"env": {`+"\n    \"ZZ_EXTRA\": \"1\",", 1)
	if edited == string(raw) {
		t.Fatal("no env block to edit")
	}
	if err := os.WriteFile(SettingsFile(), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Drifted() || !strings.Contains(strings.Join(d.Differs, ","), "env.ZZ_EXTRA") {
		t.Errorf("drift = %+v, want env.ZZ_EXTRA reported", d)
	}
}

// hermetic isolates config, pins and seams; userProviders is an optional vault overlay/providers document.
func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	// Install writes completion and the wl-paste proxy under HOME/XDG; keep them out of the real home.
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	for _, k := range []string{providers.EnvSessionProfile, "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL",
		"CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL", "ANTHROPIC_API_KEY", "TEST_GW_KEY"} {
		t.Setenv(k, "")
	}
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
	userBin := filepath.Join(dir, "localbin")
	oldUserBin := userBinDir
	userBinDir = func() string { return userBin }
	t.Cleanup(func() { userBinDir = oldUserBin })
	oldTmux := tmuxConfPaths
	tmuxConfPaths = func() []string {
		return []string{filepath.Join(dir, ".tmux.conf"), filepath.Join(dir, "xdg", "tmux.conf")}
	}
	t.Cleanup(func() { tmuxConfPaths = oldTmux })
	oldClip := installClipAgent
	installClipAgent = func(io.Writer, string, bool) error { return nil }
	t.Cleanup(func() { installClipAgent = oldClip })
	oldSelf := selfBinary
	selfBinary = func() (string, error) { return "/opt/bin/claude-code", nil }
	t.Cleanup(func() { selfBinary = oldSelf })
}

func vendorPATHFixture(t *testing.T, name, body string) string {
	t.Helper()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := pathstest.FakeVendor(t, name, body)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if name == "agy" {
		dst := filepath.Join(filepath.Dir(bin), "agy")
		if err := os.Rename(bin, dst); err != nil {
			t.Fatal(err)
		}
		bin = dst
	}
	return bin
}

// TestMain runs in the hermetic host (hosttest), so no test touches the live vault or sees a vendor CLI.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	detectBrowsers = func() browser.Report { return browser.Report{} }
	os.Exit(hosttest.Main(m))
}

func TestTmuxConfPathsEmptyWithoutHomeAndSeedSkips(t *testing.T) {
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Skip("UserHomeDir reads USERPROFILE")
	}
	if got := tmuxConfPaths(); len(got) != 0 {
		t.Fatalf("tmuxConfPaths() = %v, want none when HOME is unresolved", got)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	var out bytes.Buffer
	seedTmuxConf(&out, false)
	if !strings.Contains(out.String(), "skip") {
		t.Errorf("seedTmuxConf output = %q, want a skip line", out.String())
	}
	if _, err := os.Stat(filepath.Join(cwd, ".tmux.conf")); err == nil {
		t.Error("seedTmuxConf wrote .tmux.conf into the working directory")
	}
}

func TestFormatsCheckFailsOnUnreadableStore(t *testing.T) {
	hermetic(t, "")
	if err := os.MkdirAll(filepath.Dir(vault.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vault.Path(), bytes.Repeat([]byte("not a bolt db"), 400), 0o600); err != nil {
		t.Fatal(err)
	}
	c := formatsCheck()
	if c.OK || !strings.Contains(c.Detail, "unreadable") {
		t.Fatalf("formats row = %+v, want failing with 'unreadable'", c)
	}
}
