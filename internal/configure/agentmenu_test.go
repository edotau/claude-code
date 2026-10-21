package configure

import (
	"bufio"
	"bytes"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"strings"
	"testing"
)

// copilotKeyRow's desc flips between [no token] and [token set] as COPILOT_GITHUB_TOKEN is stored.
func TestCopilotKeyRowLabelShowsTokenState(t *testing.T) {
	menuTestEnv(t)
	d := &dialog{out: discardWriter{}}
	if got := copilotKeyRow(d).desc; !strings.Contains(got, "[no token]") {
		t.Errorf("absent: desc = %q, want [no token]", got)
	}
	if err := secrets.Store("COPILOT_GITHUB_TOKEN", "tok"); err != nil {
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
	v, src, ok := secrets.Get("COPILOT_GITHUB_TOKEN")
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
	if _, _, ok := secrets.Get("COPILOT_GITHUB_TOKEN"); ok {
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
	if err := providers.SetPins(map[string]string{providers.PinCopilotEffort: "high"}); err != nil {
		t.Fatal(err)
	}
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
	if !pickCopilotPin(d, "effort", providers.PinCopilotEffort, choices, "") {
		t.Fatal("pickCopilotPin(set) = false")
	}
	if got := providers.Pin(providers.PinCopilotEffort); got != "low" {
		t.Errorf("pin = %q, want low", got)
	}

	in = strings.NewReader("1\n") // row 1 = "unset"
	d = &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: discardWriter{}}
	if !pickCopilotPin(d, "effort", providers.PinCopilotEffort, choices, "low") {
		t.Fatal("pickCopilotPin(clear) = false")
	}
	if got := providers.Pin(providers.PinCopilotEffort); got != "" {
		t.Errorf("pin after clearing = %q, want unset", got)
	}
}

// A blank/invalid pick (Back/Esc on the non-TTY path) must leave the pin untouched.
func TestPickCopilotPinBackLeavesPinUntouched(t *testing.T) {
	menuTestEnv(t)
	if err := providers.SetPins(map[string]string{providers.PinCopilotEffort: "medium"}); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("\n")
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: discardWriter{}}
	if pickCopilotPin(d, "effort", providers.PinCopilotEffort, [][2]string{{"Low", "low"}}, "medium") {
		t.Fatal("pickCopilotPin(blank) = true, want false (Back)")
	}
	if got := providers.Pin(providers.PinCopilotEffort); got != "medium" {
		t.Errorf("pin after Back = %q, want unchanged medium", got)
	}
}

func TestRunMenuAppendsExitRowAfterAgentRows(t *testing.T) {
	reg := menuTestEnv(t)
	d := &dialog{reg: reg, out: discardWriter{}}
	_, actions := menuRows(d)
	orig := pickMenu
	t.Cleanup(func() { pickMenu = orig })
	var got [][2]string
	pickMenu = flatPick(func(_ string, rows [][2]string, _ int) (int, error) { got = rows; return -1, nil })
	runMenu(d)
	if len(got) != len(actions)+1 || got[len(actions)] != submitRow {
		t.Fatalf("rows = %v, want %d agent rows then %v", got, len(actions), submitRow)
	}
}

func TestRunMenuSubmitStoresStagedKeys(t *testing.T) {
	for _, yes := range []bool{false, true} {
		reg := menuTestEnv(t)
		var out strings.Builder
		d := &dialog{reg: reg, out: &out}
		_, actions := menuRows(d)
		origPick := pickMenu
		pickMenu = flatPick(func(string, [][2]string, int) (int, error) { // a submenu's key paste, then Submit
			storeValue(d, "GEMINI_API_KEY", "AIzaTestKey-0123456789")
			return len(actions), nil
		})
		orig := saveConfirmed
		var asked []string
		saveConfirmed = func(q string) bool { asked = append(asked, q); return yes }
		rc := runMenu(d)
		saveConfirmed, pickMenu = orig, origPick
		if rc != 0 {
			t.Fatalf("yes=%v: rc=%d", yes, rc)
		}
		if len(asked) != 1 || asked[0] != "Save 1 key(s) to the secrets manager" {
			t.Errorf("yes=%v: asked = %v", yes, asked)
		}
		v, src, _ := secrets.Get("GEMINI_API_KEY")
		if yes && (src != "vault" || v != "AIzaTestKey-0123456789") {
			t.Errorf("Submit y: GEMINI_API_KEY = %q from %q, want the staged value stored\n%s", v, src, out.String())
		}
		if !yes && src != "" {
			t.Errorf("Submit n: GEMINI_API_KEY resolves from %q, want nothing stored and the env restored", src)
		}
	}
}

// menuTestEnv pins the claude provider to subscription, so claude reads "subscription"; every other agent is unconfigured.
func TestRunMenuStatusColumnsComeFromAgentState(t *testing.T) {
	reg := menuTestEnv(t)
	rows, _ := menuRows(&dialog{reg: reg, out: discardWriter{}})
	for _, r := range rows {
		want := "not set up"
		if r[0] == "claude" {
			want = "subscription"
		}
		if !strings.Contains(r[1], want) || !strings.Contains(r[1], "not verified") {
			t.Errorf("row %v: want %q · not verified", r, want)
		}
	}
}

func fakePickAgent(t *testing.T, ans int) (title *string, start *int, rows *[][2]string) {
	t.Helper()
	orig := pickAgent
	t.Cleanup(func() { pickAgent = orig })
	title, start, rows = new(string), new(int), new([][2]string)
	pickAgent = func(ti string, r [][2]string, s int) (int, error) {
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

func TestSubmenuRowLabels(t *testing.T) {
	reg := menuTestEnv(t)
	d := &dialog{reg: reg, out: discardWriter{}}
	cases := map[string]string{
		"claude":   "Subscription,API key,Login,OAuth token",
		"codex":    "Subscription,API key,Login",
		"gemini":   "Subscription,Login",
		"opencode": "", // state only; the page is runOpencodePage
		"copilot":  "Subscription,API key,Login,User,Effort",
	}
	for name, want := range cases {
		var labels []string
		for _, r := range agentMenus[name](d).rows {
			labels = append(labels, r.label)
		}
		if got := strings.Join(labels, ","); got != want {
			t.Errorf("%s rows = %s, want %s", name, got, want)
		}
		for _, l := range labels {
			ll := strings.ToLower(l)
			if strings.Contains(ll, "chatgpt") || strings.Contains(ll, "gemini") || strings.Contains(ll, "github-copilot") {
				t.Errorf("%s submenu still lists a foreign backend row %q", name, l)
			}
		}
	}
}

// flatPick adapts a flat-rows picker stub to the sectioned home picker: rows = every section's rows, then trailing.
func flatPick(f func(string, [][2]string, int) (int, error)) func(string, []tui.Section, [2]string, int) (int, error) {
	return func(title string, secs []tui.Section, trailing [2]string, start int) (int, error) {
		var rows [][2]string
		for _, s := range secs {
			rows = append(rows, s.Rows...)
		}
		return f(title, append(rows, trailing), start)
	}
}
