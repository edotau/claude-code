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

	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
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
	orig := `{"spinnerTipsEnabled": false, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "orca stop"}]}]}}`
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
	for _, want := range []string{`"spinnerTipsEnabled": false`, `"orca stop"`, "hook Stop", "hook safety-scan"} {
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
		if _, err := paths.VendorBin(name); err != nil {
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
	if hooks.OnWSL() {
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
