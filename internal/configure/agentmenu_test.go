package configure

import (
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"strings"
	"testing"
)

func fakePickMenu(t *testing.T, answers ...int) (starts *[]int, titles *[]string) {
	t.Helper()
	orig := pickMenu
	t.Cleanup(func() { pickMenu = orig })
	var ss []int
	var ts []string
	i := 0
	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		ss, ts = append(ss, start), append(ts, title)
		a := answers[i]
		i++
		return a, nil
	})
	return &ss, &ts
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
		reg := menuTestEnv(t) // PATH has no `security`, so Store lands in the temp file plane
		var out strings.Builder
		d := &dialog{reg: reg, out: &out}
		_, actions := menuRows(d)
		origPick := pickMenu
		pickMenu = flatPick(func(string, [][2]string, int) (int, error) { // a submenu's key paste, then Submit
			storeValue(d, "GEMINI_API_KEY", "AIzaTestKey-0123456789", false)
			return len(actions), nil
		})
		orig := actionConfirmed
		var asked []string
		actionConfirmed = func(q string) bool { asked = append(asked, q); return yes }
		rc := runMenu(d)
		actionConfirmed, pickMenu = orig, origPick
		if rc != 0 {
			t.Fatalf("yes=%v: rc=%d", yes, rc)
		}
		if len(asked) != 1 || asked[0] != "Save keys to the secrets manager" {
			t.Errorf("yes=%v: asked = %v", yes, asked)
		}
		v, src, _ := secrets.Get("GEMINI_API_KEY")
		if yes && (src != "file" || v != "AIzaTestKey-0123456789") {
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
		"gemini":   "Subscription,API key,Login,Mint key",
		"opencode": "Subscription,API key,Login",
		"copilot":  "Subscription,API key,Login,OAuth token",
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
