package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini" // registers the gemini route engine the prod binary links
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type credentialProbeTest struct {
	inflight, maxSeen atomic.Int32
	gate, over        chan struct{}
	once, overOnce    sync.Once
	byPtr             map[*providers.Provider]string
}

func (s *credentialProbeTest) probe(_ context.Context, p *providers.Provider) (string, string) {
	cur := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxSeen.Load()
		if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	if cur >= credentialProbeWorkers {
		s.once.Do(func() { close(s.gate) })
	}
	if cur > credentialProbeWorkers {
		s.overOnce.Do(func() { close(s.over) })
	}
	select {
	case <-s.gate:
	case <-time.After(5 * time.Second): // only an implementation below the limit reaches this
	}
	select {
	case <-s.over:
	case <-time.After(100 * time.Millisecond): // gives a probe beyond the limit time to show up
	}
	n := s.byPtr[p]
	return "probe-" + n, "detail-" + n
}

func TestProviderCredentialChecksRunConcurrentlyInOrder(t *testing.T) {
	hermetic(t, "")
	names := []string{"p-0", "p-1", "p-2", "p-3", "p-4", "p-5", "p-6", "p-7"}
	reg := &providers.Registry{Default: "elsewhere", Providers: map[string]*providers.Provider{}}
	state := &credentialProbeTest{gate: make(chan struct{}), over: make(chan struct{}), byPtr: map[*providers.Provider]string{}}
	for _, n := range names {
		p := &providers.Provider{Auth: providers.Auth{Type: providers.AuthNone}}
		reg.Providers[n] = p
		state.byPtr[p] = n
	}
	old := probeCredential
	t.Cleanup(func() { probeCredential = old })
	probeCredential = state.probe
	rows := providerCredentialChecks(reg, providers.Selection{}, errors.New("no session"), names)
	assertCredentialProbeRows(t, rows, names)
	if m := state.maxSeen.Load(); m < 2 {
		t.Errorf("max in-flight probes = %d, want >= 2 (probes must overlap)", m)
	} else if m > credentialProbeWorkers {
		t.Errorf("max in-flight probes = %d, want <= %d (worker limit)", m, credentialProbeWorkers)
	}
}

func assertCredentialProbeRows(t *testing.T, rows []Check, names []string) {
	t.Helper()
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
		name, path string
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
			want := primary + " 1.0.0 (direct install in bin/)" + " · also on PATH: " + aBin + " 1.0.0 · also on PATH: " + bBin + " 2.0.0"
			if detail != want {
				t.Errorf("detail = %q, want %q", detail, want)
			}
		})
	}
}

func doctorVendorLink(t *testing.T, kind, source, dst string) string {
	t.Helper()
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	target := source
	want := ""
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
	return want
}

func TestDoctorVendorRows(t *testing.T) {
	for _, kind := range []string{"link", "direct", "broken link", "retired shim"} {
		t.Run(kind, func(t *testing.T) {
			doctorScratch(t)
			t.Setenv("HOME", t.TempDir())
			source := vendorPATHFixture(t, "opencode", "echo 1.2.3")
			t.Setenv("PATH", filepath.Dir(source))
			dst := pathstest.FakeVendor(t, "opencode", "echo 4.5.6")
			want := dst + " 4.5.6 (direct install in bin/)" + " · also on PATH: " + source + " 1.2.3"
			if kind != "direct" {
				want = doctorVendorLink(t, kind, source, dst)
			}
			checks, _ := Checks()
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
	checks, _ := Checks()
	c := findCheck(checks, "agent version copilot")
	if c == nil {
		t.Fatal("no `agent version copilot` row")
	}
	if !strings.HasPrefix(c.Detail, "warn 1.0.100 below 1.0.104") || c.Hard {
		t.Errorf("row = %+v, want detail `warn 1.0.100 below 1.0.104...` and Hard=false", *c)
	}
	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	checks, _ = Checks()
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

// A nested dir that holds only Claude Code's own worktrees is fine; other Keep reasons still ask for a move.
func TestNestedDetailWorktrees(t *testing.T) {
	only := setup.Nested{Dir: "/x/.claude", Files: []string{"worktrees"}, Keep: []string{setup.KeepWorktrees}}
	if !only.OnlyWorktrees() || !strings.Contains(nestedDetail(only), "holds only linked worktrees") {
		t.Fatalf("only-worktrees nested dir: %q", nestedDetail(only))
	}
	mixed := setup.Nested{Dir: "/x/.claude", Files: []string{"worktrees", "settings.json"}, Keep: []string{"settings.json:model", setup.KeepWorktrees}}
	if d := nestedDetail(mixed); mixed.OnlyWorktrees() || !strings.Contains(d, "move settings.json:model into") || strings.Contains(d, "worktrees") {
		t.Fatalf("mixed nested dir: %q", d)
	}
}

// doctorScratch isolates CLAUDE_CONFIG_DIR, every credential, and opencode's own auth.json/opencode.jsonc
// (XDG_DATA_HOME/XDG_CONFIG_HOME) so Checks never reads real machine state.
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
	checks, _ := Checks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in Checks output`)
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
	checks, _ := Checks()
	c := findCheck(checks, "opencode route")
	if c == nil {
		t.Fatal(`no "opencode route" check in Checks output`)
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
	checks, _ := Checks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in Checks output`)
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
	checks, _ := Checks()
	c := findCheck(checks, "copilot route")
	if c == nil {
		t.Fatal(`no "copilot route" check in Checks output`)
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
	t.Setenv("OPENAI_API_KEY", "nvapi-doctorFAKEkey1234567890")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"openai":{"type":"api","key":"nvapi-realFAKEsecretvalue"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := Checks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in Checks output`)
	}
	if c.OK {
		t.Errorf("OK = true, want false: a stale vault-covered key must fail the soft check")
	}
	if c.Hard {
		t.Error("the check must be soft, never hard")
	}
	if !strings.Contains(c.Detail, "openai key") || !strings.Contains(c.Detail, "opencode auth logout openai") {
		t.Errorf("detail = %q, want it to name openai and the logout command", c.Detail)
	}
	if strings.Contains(c.Detail, "nvapi-realFAKEsecretvalue") {
		t.Fatalf("detail leaked the stored credential value: %q", c.Detail)
	}
}

// No stale key (auth.json absent, or the provider has no vault credential) reports clean.
func TestDoctorOpenCodeAuthJSONCleanWhenNoStaleKey(t *testing.T) {
	doctorScratch(t)
	checks, _ := Checks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in Checks output`)
	}
	if !c.OK || c.Detail != "clean" {
		t.Errorf("check = %+v, want OK=true Detail=clean", c)
	}
}

// An oauth connection (not a static "key" method) never counts as stale, even for a vault-covered provider.
func TestDoctorOpenCodeAuthJSONIgnoresOAuthConnections(t *testing.T) {
	xdgData := doctorScratch(t)
	t.Setenv("OPENAI_API_KEY", "nvapi-doctorFAKEkey1234567890")
	authDir := filepath.Join(xdgData, "opencode")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"openai":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _ := Checks()
	c := findCheck(checks, "opencode auth.json")
	if c == nil {
		t.Fatal(`no "opencode auth.json" check in Checks output`)
	}
	if !c.OK || c.Detail != "clean" {
		t.Errorf("check = %+v, want OK=true Detail=clean for an oauth-only connection", c)
	}
}

// The doctor row never prints a secret: GH_TOKEN's value must not surface, only the env var's presence.
func TestDoctorOpenCodeRouteNamesNoSecret(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-gh-token-value")
	checks, _ := Checks()
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
	checks, _ := Checks()
	c := findCheck(checks, "credential openai")
	if c == nil || !c.OK || !strings.Contains(c.Detail, "unused") {
		t.Fatalf("unused openai row = %+v, want OK with an unused note", c)
	}
	vaulttest.SetRecord(t, "codex", "profile", "openai")
	checks, _ = Checks()
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
	if got := altAuth("anthropic"); got != "claude uses the OAuth token instead" {
		t.Errorf("altAuth(anthropic) with a headless token = %q", got)
	}
}

func TestAltAuthAnthropicNone(t *testing.T) {
	altAuthScratch(t) // no headless token, no claude binary to report a login
	if got := altAuth("anthropic"); got != "" {
		t.Errorf("altAuth(anthropic) with neither = %q, want \"\"", got)
	}
}

// gemini has no altAuth case: GeminiLoginPresent (not altAuth) now reports the agy Google login.
func TestAltAuthGemini(t *testing.T) {
	home := altAuthScratch(t)
	if got := altAuth("gemini"); got != "" {
		t.Errorf("altAuth(gemini) without the agy marker = %q, want \"\"", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := altAuth("gemini"); got != "" {
		t.Errorf("altAuth(gemini) with the agy marker = %q, want \"\" (altAuth has no gemini case)", got)
	}
}

func TestAltAuthUnknownProvider(t *testing.T) {
	altAuthScratch(t)
	if got := altAuth("openai"); got != "" {
		t.Errorf("altAuth(openai) = %q, want \"\" (no alt auth path for this provider)", got)
	}
}

// RouteDetail is the per-vendor shim `vendoragent setup` calls instead of the whole doctor sweep; its detail must
// equal the "<vendor> route" row Checks prints, for every vendor whose engine is registered.
func TestRouteDetailEqualsDoctorRouteRow(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "ghp_doctorFAKEtoken1234567890")
	checks, _ := Checks()
	for _, vendor := range []string{"codex", "gemini", "opencode", "copilot"} {
		detail, ok := agent.RouteDetail(vendor)
		c := findCheck(checks, vendor+" route")
		switch {
		case !ok:
			t.Errorf("RouteDetail(%q) ok = false, want true", vendor)
		case c == nil:
			t.Errorf("no %q row in Checks", vendor+" route")
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
// any non-tty run): "<mark> <name>: <detail>\n", mark = ✓ when OK else ✗. Doctor and PrintVault both
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

// Given sections that each wait for every other to start, Then Checks runs them at once and keeps their order.
func TestDoctorChecksRunsSectionsConcurrentlyInOrder(t *testing.T) {
	orig := doctorSections
	t.Cleanup(func() { doctorSections = orig })
	var started sync.WaitGroup
	section := func(name string, hardFail bool, delay time.Duration) doctorSection {
		return func() ([]Check, bool) {
			started.Done()
			started.Wait() // a serial runner never gets past the first section
			time.Sleep(delay)
			return []Check{{Name: name + " 1"}, {Name: name + " 2"}}, !hardFail
		}
	}
	doctorSections = []doctorSection{section("a", false, 30*time.Millisecond), section("b", true, 0),
		section("c", false, 10*time.Millisecond), section("d", false, 0)}
	started.Add(len(doctorSections))
	done := make(chan struct{})
	var checks []Check
	var ok bool
	go func() { checks, ok = Checks(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sections ran one at a time")
	}
	var got []string
	for _, c := range checks {
		got = append(got, c.Name)
	}
	want := []string{"a 1", "a 2", "b 1", "b 2", "c 1", "c 2", "d 1", "d 2"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v (section order)", got, want)
		}
	}
	if ok {
		t.Error("ok = true, want false: section b failed a hard check")
	}
}

// Doctor passes only when PATH's rtk is the pin; a shadow or a missing PATH entry fails naming the fix.
func TestRTKDoctorStatusNamesShadow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shadow fixture is an extensionless sh script, which Windows PATH lookup never finds")
	}
	pin, userBin := rtkFixture(t)
	if err := os.Symlink(pin, filepath.Join(userBin, "rtk")); err != nil {
		t.Fatal(err)
	}
	shadowDir := t.TempDir()
	mark := filepath.Join(t.TempDir(), "ran")
	writeExec(t, filepath.Join(shadowDir, "rtk"), "#!/bin/sh\n: > '"+mark+"'\necho 'rtk 0.42.4'\n")
	sep := string(os.PathListSeparator)
	for _, tc := range []struct {
		name, path, want string
		ok               bool
	}{
		{"pin first", userBin + sep + shadowDir, "PATH rtk is this binary", true},
		{"shadow first", shadowDir + sep + userBin, "PATH rtk " + filepath.Join(shadowDir, "rtk") + " shadows " +
			pin + " — run make install, or put ~/.local/bin first on PATH", false},
		{"none on PATH", t.TempDir(), pin + " is not on PATH — run make install", false},
	} {
		t.Setenv("PATH", tc.path)
		ok, detail := rtkDoctorStatus()
		if ok != tc.ok || !strings.Contains(detail, tc.want) {
			t.Errorf("%s: (%v, %q), want (%v, …%q…)", tc.name, ok, detail, tc.ok, tc.want)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("doctor executed the shadowing rtk")
	}
}

func TestToolDoctorRejectsLstatFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	probeSymlinkLoop(t, paths.BinDir())
	entry := filepath.Join(paths.BinDir(), "opencode")
	if _, err := os.Lstat(entry); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("fixture Lstat = %v, want ELOOP", err)
	}
	if toolDoctorOK(tools.Status{Name: "opencode"}) {
		t.Fatal("tool doctor reported healthy despite non-ENOENT Lstat failure")
	}
}

func TestToolHealthPreservesExhaustedQuotaFailure(t *testing.T) {
	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	now := time.Now()
	if err := router.Record("github·regression", 100, "test", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	var rows []Check
	checkToolHealth(func(ok, hard bool, name, detail string) {
		rows = append(rows, Check{Name: name, OK: ok, Hard: hard, Detail: detail})
	})
	row := findCheck(rows, "quota github·regression")
	if row == nil {
		t.Fatal("missing exhausted quota row")
	}
	if row.OK || row.Hard || !strings.Contains(row.Detail, "exhausted at 100%") {
		t.Fatalf("exhausted quota row = %+v, want failed soft check with exhaustion detail", row)
	}
}

// A loop produces a deterministic non-ENOENT probe failure even when tests run as root.
func probeSymlinkLoop(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a self-referencing symlink (ELOOP) needs unix")
	}
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("fixture stat = %v, want ELOOP", err)
	}
}

// hermetic isolates config and the env a doctor row reads; userProviders is an optional vault overlay/providers document.
func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
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

// rtkFixture installs a pinned rtk under a temp CLAUDE_CONFIG_DIR and returns it with an empty user bin dir.
func rtkFixture(t *testing.T) (pin, userBin string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(paths.RTKBinaryEnv, "")
	pin = paths.RTKPinned()
	writeExec(t, pin, "#!/bin/sh\necho 'rtk 1.1.0'\n")
	return pin, t.TempDir()
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
