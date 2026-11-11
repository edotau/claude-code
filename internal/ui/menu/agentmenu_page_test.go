package menu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// copilotKeyRow's desc flips between [no token] and [token set] as COPILOT_GITHUB_TOKEN is stored.
func TestCopilotKeyRowLabelShowsTokenState(t *testing.T) {
	menuTestEnv(t)
	d := &dialog{out: discardWriter{}}
	if got := copilotKeyRow(d).desc; !strings.Contains(got, "[no token]") {
		t.Errorf("absent: desc = %q, want [no token]", got)
	}
	if err := vault.Store("COPILOT_GITHUB_TOKEN", "tok"); err != nil {
		t.Fatal(err)
	}
	if got := copilotKeyRow(d).desc; !strings.Contains(got, "[token set]") {
		t.Errorf("present: desc = %q, want [token set]", got)
	}
}

// With no token stored, selecting copilot's API key row pastes straight into COPILOT_GITHUB_TOKEN (the
// non-TTY dialogue path) — no picker, and it must never touch the anthropic console.
func TestCopilotKeyRowAbsentPastesDirectlyNoAnthropicConsole(t *testing.T) {
	menuTestEnv(t)
	var out bytes.Buffer
	const pasted = "gh" + "p_pastedFAKEtoken1234567890" // valid shape: ValidCopilotToken requires a gh prefix, >=20 chars
	in := strings.NewReader(pasted + "\n")
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	if !copilotKeyRow(d).run() {
		t.Fatalf("copilotKeyRow(d).run() = false:\n%s", out.String())
	}
	v, src, ok := vault.Get("COPILOT_GITHUB_TOKEN")
	if !ok || v != pasted {
		t.Fatalf("COPILOT_GITHUB_TOKEN = (%q, %q, %v), want %s stored", v, src, ok, pasted)
	}
	if strings.Contains(out.String(), "anthropic") {
		t.Errorf("absent-token paste must never mention the anthropic console:\n%s", out.String())
	}
}

// A paste that does not look like a real GitHub token is refused: the row reports failure and
// nothing is written to the vault.
func TestCopilotKeyRowAbsentPasteRefusesShortToken(t *testing.T) {
	menuTestEnv(t)
	var out bytes.Buffer
	in := strings.NewReader("ok\n")
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	if copilotKeyRow(d).run() {
		t.Fatalf("copilotKeyRow(d).run() = true for a short paste, want refused:\n%s", out.String())
	}
	if _, _, ok := vault.Get("COPILOT_GITHUB_TOKEN"); ok {
		t.Fatal("a refused paste must not store anything")
	}
	if !strings.Contains(out.String(), "not a GitHub token") {
		t.Errorf("output = %q, want the not-a-GitHub-token refusal", out.String())
	}
}

// copilotContextEligible is the claude-* model gate for the Context row when the SDK catalog is
// unreachable (menuTestEnv's PATH has no copilot binary, so copilotCatalogModel always misses): a
// Claude-backed opus slot is eligible, anything else (an "auto" alias, a GPT id) is not.
func TestCopilotContextEligible(t *testing.T) {
	menuTestEnv(t)
	cases := map[string]bool{"claude-opus-5.5": true, "claude-sonnet-5": true, "auto": false, "gpt-5": false, "": false}
	for model, want := range cases {
		d := &dialog{out: discardWriter{}}
		if got := copilotContextEligible(d, model); got != want {
			t.Errorf("copilotContextEligible(%q) = %v, want %v", model, got, want)
		}
	}
}

// copilotContextRow is nil for an ineligible model (no long-context tier to pin) and present for one,
// same claude-* fallback as copilotContextEligible since the SDK catalog is unreachable here.
func TestCopilotContextRowNilWhenIneligible(t *testing.T) {
	menuTestEnv(t)
	d := &dialog{out: discardWriter{}}
	if r := copilotContextRow(d, "auto"); r != nil {
		t.Errorf("ineligible model: row = %+v, want nil", r)
	}
	if r := copilotContextRow(d, "claude-opus-5.5"); r == nil {
		t.Error("eligible model: row = nil, want the Context row")
	}
}

// copilotEffortRow's desc flips between "unset" and the pinned value as COPILOT_REASONING_EFFORT
// changes; the SDK catalog is unreachable here, so the row falls back to the fixed effort list.
func TestCopilotEffortRowShowsPinState(t *testing.T) {
	menuTestEnv(t)
	d := &dialog{out: discardWriter{}}
	if got := copilotEffortRow(d, "auto").desc; !strings.Contains(got, "unset") {
		t.Errorf("unpinned: desc = %q, want unset", got)
	}
	vaulttest.SetRecord(t, "copilot", "effort", "high")
	if got := copilotEffortRow(d, "auto").desc; !strings.Contains(got, "high") {
		t.Errorf("pinned: desc = %q, want high", got)
	}
}

// pickCopilotPin, on the non-TTY numbered-prompt path: picking a choice pins its value; picking "unset"
// clears a previously-set pin. Row 0 is always "unset", so row N+1 is choices[N].
func TestPickCopilotPinSetsAndClearsPin(t *testing.T) {
	menuTestEnv(t)
	choices := [][2]string{{"Low", "low"}, {"High", "high"}}

	in := strings.NewReader("2\n") // row 2 = choices[0] = "low"
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: discardWriter{}}
	if !pickCopilotPin(d, "effort", "EFFORT", choices, "") {
		t.Fatal("pickCopilotPin(set) = false")
	}
	if got := vault.GetAgentRecord("copilot").Effort; got != "low" {
		t.Errorf("pin = %q, want low", got)
	}

	in = strings.NewReader("1\n") // row 1 = "unset"
	d = &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: discardWriter{}}
	if !pickCopilotPin(d, "effort", "EFFORT", choices, "low") {
		t.Fatal("pickCopilotPin(clear) = false")
	}
	if got := vault.GetAgentRecord("copilot").Effort; got != "" {
		t.Errorf("pin after clearing = %q, want unset", got)
	}
}

// A blank/invalid pick (Back/Esc on the non-TTY path) must leave the pin untouched.
func TestPickCopilotPinBackLeavesPinUntouched(t *testing.T) {
	menuTestEnv(t)
	vaulttest.SetRecord(t, "copilot", "effort", "medium")
	in := strings.NewReader("\n")
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: discardWriter{}}
	if pickCopilotPin(d, "effort", "EFFORT", [][2]string{{"Low", "low"}}, "medium") {
		t.Fatal("pickCopilotPin(blank) = true, want false (Back)")
	}
	if got := vault.GetAgentRecord("copilot").Effort; got != "medium" {
		t.Errorf("pin after Back = %q, want unchanged medium", got)
	}
}

func fakePickAgent(t *testing.T, ans int) (title *string, start *int, rows *[][2]string) {
	t.Helper()
	orig := pickAgent
	t.Cleanup(func() { pickAgent = orig })
	title, start, rows = new(string), new(int), new([][2]string)
	pickAgent = func(_ *dialog, ti string, r [][2]string, s int) (int, error) {
		*title, *rows, *start = ti, r, s
		return ans, nil
	}
	return
}

func testMenu(ran *[]string, st agentState) agentMenu {
	mk := func(label, key string) billingRow {
		return billingRow{label, "d", key, func() bool { *ran = append(*ran, label); return true }}
	}
	return agentMenu{st, []billingRow{mk("Subscription", modeSubscription), mk("API key", modeAPIKey), mk("Login", "")}}
}

func TestRunAgentMenuTitleAndStart(t *testing.T) {
	cases := []struct {
		name      string
		st        agentState
		wantTitle string
		wantStart int
	}{
		{"verified sub", agentState{mode: modeSubscription, verified: true}, "x (current: subscription · verified)", 0},
		{"unverified key", agentState{mode: modeAPIKey}, "x (current: API key)", 1},
		{"not set up", agentState{mode: modeNone}, "x (current: not set up)", 0},
		{"foreign backend label", agentState{mode: "openai (not anthropic)", verified: true}, "x (current: openai (not anthropic) · verified)", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, start, _ := fakePickAgent(t, -1)
			var ran []string
			runAgentMenu(&dialog{out: discardWriter{}}, "x", testMenu(&ran, c.st))
			if *title != c.wantTitle || *start != c.wantStart {
				t.Errorf("title=%q start=%d, want %q / %d", *title, *start, c.wantTitle, c.wantStart)
			}
		})
	}
}

func TestRunAgentMenuBackRunsNothingAndPickRunsThatRow(t *testing.T) {
	var ran []string
	fakePickAgent(t, -1)
	if runAgentMenu(&dialog{out: discardWriter{}}, "x", testMenu(&ran, agentState{})) || len(ran) != 0 {
		t.Fatalf("Back ran %v", ran)
	}
	fakePickAgent(t, 2)
	if !runAgentMenu(&dialog{out: discardWriter{}}, "x", testMenu(&ran, agentState{})) || len(ran) != 1 || ran[0] != "Login" {
		t.Fatalf("ran = %v, want [Login]", ran)
	}
}

// Every agent page but opencode's offers one subscription row, at most one API-key row, distinct runnable rows.
func TestSubmenuRowLabels(t *testing.T) {
	reg := menuTestEnv(t)
	d := &dialog{reg: reg, out: discardWriter{}}
	for name, build := range agentMenus {
		rows := build(d).rows
		if name == "opencode" {
			if len(rows) != 0 {
				t.Errorf("opencode rows = %d, want none: its page is runOpencodePage", len(rows))
			}
			continue
		}
		assertAgentPageRows(t, name, rows)
	}
}

// assertAgentPageRows requires unique runnable rows, exactly one keyed subscription, at most one keyed API key.
func assertAgentPageRows(t *testing.T, name string, rows []billingRow) {
	t.Helper()
	labels, keys := map[string]bool{}, map[string]int{}
	for _, r := range rows {
		if labels[r.label] || r.run == nil {
			t.Errorf("%s row %q is a duplicate or runs nothing", name, r.label)
		}
		labels[r.label] = true
		keys[r.key]++
		if l := strings.ToLower(r.label); strings.Contains(l, "chatgpt") || strings.Contains(l, "gemini") ||
			strings.Contains(l, "github-copilot") {
			t.Errorf("%s page lists a foreign backend row %q", name, r.label)
		}
	}
	sub, api := keys[modeSubscription], keys[modeAPIKey]
	delete(keys, "")
	delete(keys, modeSubscription)
	delete(keys, modeAPIKey)
	if sub != 1 || api > 1 || len(keys) > 0 {
		t.Errorf("%s: %d subscription, %d API-key rows, other mode keys %v; want 1, <=1, none", name, sub, api, keys)
	}
}

func TestAgentStateLabelAndCheck(t *testing.T) {
	cases := []struct {
		s            agentState
		label, check string
	}{
		{agentState{modeSubscription, providers.Subscription{Name: "Max 20x"}, true}, "subscription (Max 20x)", "verified"},
		{agentState{mode: modeSubscription}, "subscription", "not verified"},
		{agentState{mode: modeAPIKey, tier: providers.Subscription{Name: "ignored"}, verified: true}, "API key", "verified"},
		{agentState{mode: modeNone}, "not set up", "not verified"},
	}
	for _, c := range cases {
		if c.s.label() != c.label || c.s.check() != c.check {
			t.Errorf("%+v: label=%q check=%q, want %q/%q", c.s, c.s.label(), c.s.check(), c.label, c.check)
		}
	}
}

// Each codex Login pick runs its own sign-in: browser, device code, or (cancelled) nothing.
func TestCodexLoginPicksMethod(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"1\n", "login\n"}, {"2\n", "login --device-auth\n"}, {"\n", ""}} {
		cfg := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", cfg)
		log := filepath.Join(cfg, "argv")
		pathstest.FakeVendor(t, "codex", "#!/bin/sh\necho \"$*\" >> "+log+"\n")
		d := &dialog{out: discardWriter{}, br: bufio.NewReader(strings.NewReader(tc.in))}
		codexLogin(d, func() bool { t.Error("API key path ran"); return false })
		got, _ := os.ReadFile(log)
		if string(got) != tc.want {
			t.Errorf("pick %q ran %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The API key pick pipes the vault's OPENAI_API_KEY to `codex login --with-api-key`, and only once ensureKey succeeds.
func TestCodexLoginAPIKeyPipesVaultKey(t *testing.T) {
	for _, tc := range []struct {
		ensure bool
		want   string
	}{{true, "login --with-api-key stdin=sk-vault\n"}, {false, ""}} {
		cfg := vaulttest.Use(t)
		if err := vault.Store("OPENAI_API_KEY", "sk-vault"); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(cfg, "argv")
		pathstest.FakeVendor(t, "codex", "#!/bin/sh\necho \"$* stdin=$(cat)\" >> "+log+"\n")
		d := &dialog{out: discardWriter{}, br: bufio.NewReader(strings.NewReader("3\n"))}
		if got := codexLogin(d, func() bool { return tc.ensure }); got != tc.ensure {
			t.Errorf("ensure=%v: codexLogin = %v", tc.ensure, got)
		}
		got, _ := os.ReadFile(log)
		if string(got) != tc.want {
			t.Errorf("ensure=%v ran %q, want %q", tc.ensure, got, tc.want)
		}
	}
}

var setupAgentNames = []string{"claude", "codex", "gemini", "opencode", "copilot"}

// setupFake swaps every Set up seam; calls records the step order, the errors fail that step.
type setupFake struct {
	calls                      []string
	verified                   bool
	loginErr, verifyErr, rtErr error
	changed                    bool
}

func installSetupFake(t *testing.T, f *setupFake) {
	t.Helper()
	st, ve, ro := setupState, setupVerify, setupRoute
	logins := map[string]func(*dialog) error{}
	for k, v := range setupLogins {
		logins[k] = v
		setupLogins[k] = func(*dialog) error { f.calls = append(f.calls, "login"); return f.loginErr }
	}
	t.Cleanup(func() {
		setupState, setupVerify, setupRoute = st, ve, ro
		maps.Copy(setupLogins, logins)
	})
	setupState = func(*dialog, string) agentState { return agentState{verified: f.verified} }
	setupVerify = func(context.Context, string) error { f.calls = append(f.calls, "verify"); return f.verifyErr }
	setupRoute = func(*dialog, string) (bool, error) { f.calls = append(f.calls, "route"); return f.changed, f.rtErr }
}

func setupDialog(t *testing.T) (*dialog, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	return &dialog{reg: menuTestEnv(t), out: out}, out
}

func TestAgentSetUpOrderAndStops(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name        string
		f           setupFake
		calls, want string
		notWant     string
	}{
		{"success", setupFake{changed: true}, "login,verify,route", "verified", "stopped"},
		{"already verified skips sign-in", setupFake{verified: true, changed: true}, "verify,route", "already signed in", "stopped"},
		{"sign-in fails", setupFake{loginErr: boom}, "login", "set up stopped at sign-in: boom", "verified"},
		{"verify fails", setupFake{verifyErr: boom}, "login,verify", "set up stopped at verify: boom", "route was not"},
		{"route fails", setupFake{rtErr: boom}, "login,verify,route", "set up stopped at route: boom", "route was not"},
		{"route cancelled", setupFake{}, "login,verify,route", "signed in; the route was not changed", "stopped"},
	} {
		for _, agent := range setupAgentNames {
			t.Run(c.name+"/"+agent, func(t *testing.T) {
				d, out := setupDialog(t)
				f := c.f
				installSetupFake(t, &f)
				agentSetUp(d, agent)
				if got := strings.Join(f.calls, ","); got != c.calls {
					t.Errorf("calls = %s, want %s", got, c.calls)
				}
				prefix := displayLabel(agent) + ": "
				if !strings.Contains(out.String(), prefix) || !strings.Contains(out.String(), c.want) || strings.Contains(out.String(), c.notWant) {
					t.Errorf("out = %q, want %q%q without %q", out.String(), prefix, c.want, c.notWant)
				}
			})
		}
	}
}

func TestAgentSetUpCopilotCreditsReadAsNotLoginFailure(t *testing.T) {
	d, out := setupDialog(t)
	installSetupFake(t, &setupFake{verifyErr: errors.New("status quota")})
	agentSetUp(d, "copilot")
	for _, w := range []string{"copilot: set up stopped at verify: signed in, but copilot has no credits left", "not a login failure"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("out = %q, want %q", out.String(), w)
		}
	}
	d2, out2 := setupDialog(t)
	installSetupFake(t, &setupFake{verifyErr: errors.New("status quota")})
	agentSetUp(d2, "codex")
	if strings.Contains(out2.String(), "credits") {
		t.Errorf("only copilot gets the credits message: %q", out2.String())
	}
}

func TestAgentSetUpNeverPrintsAToken(t *testing.T) {
	tok := "Bearer " + strings.Repeat("s3cr3tTOKEN", 5)
	d, out := setupDialog(t)
	installSetupFake(t, &setupFake{verifyErr: errors.New("401 with " + tok)})
	agentSetUp(d, "codex")
	if !strings.Contains(out.String(), "set up stopped at verify") || strings.Contains(out.String(), "s3cr3tTOKEN") {
		t.Errorf("out = %q", out.String())
	}
}

func TestSetUpIsEveryAgentPagesFirstRow(t *testing.T) {
	for _, agent := range setupAgentNames {
		d, _ := setupDialog(t)
		f := &setupFake{changed: true}
		installSetupFake(t, f)
		_, _, rows := fakePickAgent(t, 0)
		runAgentMenu(d, displayLabel(agent), agentMenus[agent](d))
		if got := (*rows)[0]; got != [2]string{"Set up", "sign in, verify, pin the route"} {
			t.Errorf("%s first row = %v", agent, got)
		}
		if strings.Join(f.calls, ",") != "login,verify,route" {
			t.Errorf("%s calls = %v", agent, f.calls)
		}
		if !slices.ContainsFunc(agentPinRows(d, agent), func(r billingRow) bool { return r.label == "Profile" }) {
			t.Errorf("%s has no Profile row for the configure step", agent)
		}
	}
}

// runRealRoute drives Set up's real route step (picker + pin write) after a verified agent, answering the picker with in.
func runRealRoute(t *testing.T, in string, readOnlyVault bool) string {
	t.Helper()
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt") // codex is on its own login, so picking off is a real write
	if readOnlyVault {
		if err := os.Chmod(vault.Path(), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(vault.Path(), 0o600) })
	}
	st, ve := setupState, setupVerify
	t.Cleanup(func() { setupState, setupVerify = st, ve })
	setupState = func(*dialog, string) agentState { return agentState{verified: true} }
	setupVerify = func(context.Context, string) error { return nil }
	var out bytes.Buffer
	d := &dialog{reg: reg, out: &out, br: bufio.NewReader(strings.NewReader(in))}
	agentSetUp(d, "codex")
	return out.String()
}

// A pin write that fails must end Set up with "stopped at route", not read as a dismissed picker.
func TestSetUpRouteFailedPinWriteIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	got := runRealRoute(t, "1\n", true)
	if !strings.Contains(got, "set up stopped at route:") || strings.Contains(got, "the route was not changed") {
		t.Errorf("failed write: out = %q, want 'stopped at route' and no 'not changed'", got)
	}
}

// A dismissed picker stays quiet: only "the route was not changed", no error.
func TestSetUpRouteCancelIsQuiet(t *testing.T) {
	got := runRealRoute(t, "\n", false)
	if !strings.Contains(got, "signed in; the route was not changed") || strings.Contains(got, "stopped at") {
		t.Errorf("cancel: out = %q, want only 'route was not changed'", got)
	}
}

// fakeAgyCommand replaces agyCommand with a shell script: setup gives it a chance to touch the marker
// (or not) before it exits, so each case drives runAgy without a real `agy` binary.
func fakeAgyCommand(t *testing.T, script string) {
	t.Helper()
	orig := configure.AgyCommand
	t.Cleanup(func() { configure.AgyCommand = orig })
	configure.AgyCommand = func() *exec.Cmd { return exec.Command("/bin/sh", "-c", script) }
}

// TestRunAgySignsInThenStopsIt: agy writes its marker after a beat and then just sits there (a stand-in
// for its own TUI staying up); runAgy must notice the marker and stop the process, not wait for its exit.
func TestRunAgySignsInThenStopsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeAgyCommand(t, "sleep 0.2; echo tok > '"+marker+"'; exec sleep 30")
	origPoll, origGrace := configure.AgySignInPoll, configure.AgyStopGrace
	t.Cleanup(func() { configure.AgySignInPoll, configure.AgyStopGrace = origPoll, origGrace })
	configure.AgySignInPoll, configure.AgyStopGrace = 50*time.Millisecond, 100*time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	done := make(chan bool, 1)
	go func() { done <- runAgy(d) }()
	select {
	case ok := <-done:
		if !ok {
			t.Errorf("runAgy = false, want true (marker appeared): %s", out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAgy did not return once the marker appeared")
	}
	if !strings.Contains(out.String(), "signed in") {
		t.Errorf("output = %q, want a signed-in line", out.String())
	}
}

// TestRunAgyReportsNotSignedInWhenItExitsOnItsOwn: agy exits without ever writing the marker.
func TestRunAgyReportsNotSignedInWhenItExitsOnItsOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeAgyCommand(t, "exit 1")
	t.Cleanup(func() { configure.AgySignInPoll = 500 * time.Millisecond })
	configure.AgySignInPoll = 20 * time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	if runAgy(d) {
		t.Errorf("runAgy = true, want false: agy exited without ever signing in (%s)", out.String())
	}
	if !strings.Contains(out.String(), "not signed in") {
		t.Errorf("output = %q, want a not-signed-in line", out.String())
	}
}

// TestRunAgyStopsOnTimeout: agy never writes the marker and never exits; runAgy must give up at
// configure.AgySignInTimeout and stop it rather than wait forever.
func TestRunAgyStopsOnTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeAgyCommand(t, "exec sleep 30")
	origPoll, origTimeout, origGrace := configure.AgySignInPoll, configure.AgySignInTimeout, configure.AgyStopGrace
	t.Cleanup(func() {
		configure.AgySignInPoll, configure.AgySignInTimeout, configure.AgyStopGrace = origPoll, origTimeout, origGrace
	})
	configure.AgySignInPoll, configure.AgySignInTimeout, configure.AgyStopGrace = 10*time.Millisecond, 60*time.Millisecond, 20*time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	done := make(chan bool, 1)
	go func() { done <- runAgy(d) }()
	select {
	case ok := <-done:
		if ok {
			t.Errorf("runAgy = true, want false (timed out): %s", out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAgy did not return at the timeout")
	}
	if !strings.Contains(out.String(), "timed out") {
		t.Errorf("output = %q, want a timed-out line", out.String())
	}
}

func TestGeminiMenuRowLabels(t *testing.T) {
	reg := menuTestEnv(t)
	m := geminiMenu(&dialog{reg: reg, out: discardWriter{}})
	var got []string
	for _, r := range m.rows {
		got = append(got, r.label)
	}
	if want := "Subscription,Login"; strings.Join(got, ",") != want {
		t.Errorf("gemini rows = %v, want %s", got, want)
	}
	if m.state.mode != modeNone {
		t.Errorf("state = %+v, want %s with nothing configured", m.state, modeNone)
	}
}

// copilotEffortRow lists only the catalog model's own supported efforts, with its DefaultEffort marked.
func TestCopilotEffortRowListsModelsOwnEfforts(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
		{ID: "claude-sonnet-5", ReasoningEffort: true, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"},
	}, nil)

	d := &dialog{out: discardWriter{}}
	r := copilotEffortRow(d, "claude-sonnet-5")
	if r == nil {
		t.Fatal("copilotEffortRow = nil, want a row (model supports reasoning effort)")
	}
	in := strings.NewReader("\n") // blank = cancel; we only need the printed choice list
	var out bytes.Buffer
	d2 := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	r2 := copilotEffortRow(d2, "claude-sonnet-5")
	r2.run()
	shown := out.String()
	if !strings.Contains(shown, "Medium (default)") {
		t.Errorf("output missing the default marker on medium:\n%s", shown)
	}
	if strings.Contains(shown, "Extra High") || strings.Contains(shown, "Max") {
		t.Errorf("output lists an effort the model does not support:\n%s", shown)
	}
}

// Switching to a model that cannot honor the current effort/context pins must clear them, with a reason.
func TestClearIneligibleCopilotPinsOnModelSwitch(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
		{ID: "new-model", ReasoningEffort: true, Efforts: []string{"low", "medium"}, ContextTokens: 128_000},
	}, nil)
	vaulttest.SetRecord(t, "copilot", "effort", "high")
	vaulttest.SetRecord(t, "copilot", "context", "long_context")

	var out bytes.Buffer
	d := &dialog{out: &out}
	clearIneligibleCopilotPins(d, "new-model")

	if got := vault.GetAgentRecord("copilot").Effort; got != "" {
		t.Errorf("effort pin = %q, want cleared (new-model doesn't support high)", got)
	}
	if got := vault.GetAgentRecord("copilot").Context; got != "" {
		t.Errorf("context pin = %q, want cleared (new-model has no long-context tier)", got)
	}
	if !strings.Contains(out.String(), "doesn't support that effort") || !strings.Contains(out.String(), "has no long-context tier") {
		t.Errorf("output missing the clear reasons:\n%s", out.String())
	}
}

// clearIneligibleCopilotPins must leave compatible pins alone: a model that still supports the pinned
// effort and still has a long-context tier keeps both.
func TestClearIneligibleCopilotPinsKeepsCompatiblePins(t *testing.T) {
	menuTestEnv(t)
	fakeCopilotBin(t)
	stubCatalog(t, []copilot.SDKModel{
		{ID: "still-fits", ReasoningEffort: true, Efforts: []string{"low", "high"}, ContextTokens: 1_000_000},
	}, nil)
	vaulttest.SetRecord(t, "copilot", "effort", "high")
	vaulttest.SetRecord(t, "copilot", "context", "long_context")

	d := &dialog{out: discardWriter{}}
	clearIneligibleCopilotPins(d, "still-fits")

	if got := vault.GetAgentRecord("copilot").Effort; got != "high" {
		t.Errorf("effort pin = %q, want unchanged high", got)
	}
	if got := vault.GetAgentRecord("copilot").Context; got != "long_context" {
		t.Errorf("context pin = %q, want unchanged long_context", got)
	}
}

// TestRunAgyStopsDescendants: a non-exec shell spawning a background child (stand-in for Node spawning a
// child process) must have its descendant killed by stopAgy when the sign-in marker appears.
func TestRunAgyStopsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group stop needs POSIX signals and a POSIX shell")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pidFileSh := filepath.Join(dir, "sh.pid")
	pidFile := filepath.Join(dir, "child.pid")
	// Marker last: a slow runner's poll must not kill the shell before the child and its pidfile exist.
	script := "echo $$ > '" + pidFileSh + "'; sleep 30 & echo $! > '" + pidFile + "'; sleep 0.1; echo tok > '" + marker + "'; wait"
	fakeAgyCommand(t, script)
	origPoll, origGrace := configure.AgySignInPoll, configure.AgyStopGrace
	t.Cleanup(func() { configure.AgySignInPoll, configure.AgyStopGrace = origPoll, origGrace })
	configure.AgySignInPoll, configure.AgyStopGrace = 50*time.Millisecond, 50*time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	if !runAgy(d) {
		t.Fatalf("runAgy = false, want true: %s", out.String())
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid child pid %q: %v", raw, err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return // already gone
	}
	t.Cleanup(func() { _ = proc.Kill() })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("child pid %d still alive after runAgy returned", pid)
}
