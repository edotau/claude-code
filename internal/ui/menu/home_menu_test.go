package menu

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/table"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// menuTestEnv isolates config/home/PATH so keyTargets' vendor probes never touch a real login CLI or
// the real keyring, and loads the real registry on the subscription provider (no live network calls).
func menuTestEnv(t *testing.T) *providers.Registry {
	t.Helper()
	origExtra := extraSections
	t.Cleanup(func() { extraSections = origExtra })
	extraSections = func(*dialog) ([]tui.Section, []func(*dialog)) { return nil, nil }
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir()) // a foreign tool may export its own codex account home; codex must read unconfigured
	t.Setenv("PATH", t.TempDir())
	vaulttest.SetRecord(t, "claude", "profile", "ultra") // seed_test clears this pin to start first-run
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	// Copilot/codex/gemini pin env beats the vault pin (providers.Pin); a session running inside one of
	// these agents exports its own model/effort pin into the process env.
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "ANTHROPIC_API_KEY",
		"COPILOT_MODEL", "COPILOT_EFFORT", "COPILOT_CONTEXT", "CODEX_MODEL", "CODEX_EFFORT", "GEMINI_MODEL"} {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Skip(err)
	}
	return reg
}

// TestMenuRowsOrderNoSeparateProviderRow: the top-level menu is claude, then every agent.Names() CLI
// in roster order, then Exit (appended by runMenu, not menuRows); sections are `configure <section>` args — "provider" is
// folded into the claude row and must not appear again as its own entry.
func TestMenuRowsOrderNoSeparateProviderRow(t *testing.T) {
	reg := menuTestEnv(t)
	rows, actions := menuRows(&dialog{reg: reg, out: discardWriter{}})
	if len(rows) != len(actions) {
		t.Fatalf("rows=%d actions=%d, must match 1:1", len(rows), len(actions))
	}
	var labels []string
	for _, r := range rows {
		labels = append(labels, r[0])
	}
	want := []string{"claude", "codex", "copilot", "antigravity", "opencode"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Fatalf("menu rows = %v, want %v", labels, want)
	}
	for _, l := range labels {
		if l == "provider" || l == "Provider" {
			t.Fatalf("a separate provider row must not exist alongside claude: %v", labels)
		}
	}
}

// TestMenuRowsAgentScopeShowsOnlyThatRow: --agent codex narrows the whole menu to the codex row alone.
func TestMenuRowsAgentScopeShowsOnlyThatRow(t *testing.T) {
	reg := menuTestEnv(t)
	rows, actions := menuRows(&dialog{reg: reg, o: Options{Agent: "codex"}, out: discardWriter{}})
	if len(rows) != 1 || rows[0][0] != "codex" || len(actions) != 1 {
		t.Fatalf("rows = %v, want exactly one row: codex", rows)
	}
}

// TestRunMenuDoneLeavesImmediatelyAndChangesNothing: Esc (, which pickMenu reports the same way
// as -1) must run no action and leave the pinned provider untouched.
func TestRunMenuDoneLeavesImmediatelyAndChangesNothing(t *testing.T) {
	reg := menuTestEnv(t)
	origMenu := pickMenu
	t.Cleanup(func() { pickMenu = origMenu })
	calls := 0
	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		calls++
		return -1, nil // Esc
	})
	cur := vault.GetAgentRecord("claude").Profile
	rc := runMenu(&dialog{reg: reg, out: discardWriter{}})
	if rc != 0 || calls != 1 {
		t.Fatalf("rc=%d calls=%d, want rc=0 after exactly one picker call", rc, calls)
	}
	if got := vault.GetAgentRecord("claude").Profile; got != cur {
		t.Errorf("provider pin changed: %q -> %q", cur, got)
	}
}

// TestCheckAndPipedNeverOpenTheMenu: --check and a piped run keep the old Sections walk; the top-level
// picker must never be invoked, regardless of how pickMenu would answer.
func TestCheckAndPipedNeverOpenTheMenu(t *testing.T) {
	menuTestEnv(t)
	origMenu := pickMenu
	t.Cleanup(func() { pickMenu = origMenu })
	calls := 0
	pickMenu = flatPick(func(title string, rows [][2]string, start int) (int, error) {
		calls++
		return -1, nil
	})
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "")
	if calls != 0 {
		t.Fatalf("--check invoked the menu picker %d times, want 0", calls)
	}
	if !strings.Contains(out.String(), "== provider ==") || strings.Contains(out.String(), "== keys ==") {
		t.Errorf("--check output must still be the Sections walk, without a keys section:\n%s", out.String())
	}
	_ = rc
	out.Reset()
	_ = Run(Options{In: strings.NewReader(""), Out: &out}, "") // piped: Interactive left false
	if calls != 0 {
		t.Fatalf("a piped run invoked the menu picker %d times, want 0", calls)
	}
	if !strings.Contains(out.String(), "== provider ==") || strings.Contains(out.String(), "== keys ==") {
		t.Errorf("piped output must still be the Sections walk, without a keys section:\n%s", out.String())
	}
}

// TestKeepOrPinWritesTheProviderPinOnlyOnAnExplicitDifferentPick: the mechanism every claude
// sub-picker row relies on to apply a choice — keepOrPin — must write the pin only when the picked
// name differs from the one already in effect; picking the current backend again must not re-pin it.
//
// This is the closest unit-level proof available for "picking claude pins only on an explicit backend
// pick": runAgentMenu's picker is now the pickAgent seam (see agentmenu_page_test.go).
func TestKeepOrPinWritesTheProviderPinOnlyOnAnExplicitDifferentPick(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	d := &dialog{out: discardWriter{}}
	if !keepOrPin(d, "subscription", "subscription") {
		t.Fatal("keepOrPin(same) = false")
	}
	if got := vault.GetAgentRecord("claude").Profile; got != "" {
		t.Errorf("vault pin written on a same-as-current pick: %q", got)
	}
	if !keepOrPin(d, "subscription", "anthropic") {
		t.Fatal("keepOrPin(different) = false")
	}
	if got := vault.GetAgentRecord("claude").Profile; got != "anthropic" {
		t.Errorf("claude profile = %q, want %q after an explicit different pick", got, "anthropic")
	}
}

// assertNotInVault requires name to be absent from the vault's secrets.
func assertNotInVault(t *testing.T, name string) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		if _, found := tx.Get(vault.Secrets, name); found {
			t.Errorf("the failed key must not end up in the vault")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSaveKeysPartialFailureKeepsFailedStagedOthersDiscarded: a Store failure (here, a multi-line
// value vault.Store genuinely refuses — no mock) must leave only the failed name staged, with its
// shadowing env still set, while every name that did save is dropped from pending and its env restored.
// Teeth: fixing the bad value and rerunning saveKeys flips it to true with pending left empty.
func TestSaveKeysPartialFailureKeepsFailedStagedOthersDiscarded(t *testing.T) {
	vaulttest.Use(t)
	const bad, good = "CONFIGURE_SAVEKEYS_BAD", "CONFIGURE_SAVEKEYS_GOOD"
	badVal := "line one\nline two" // vault.Store rejects a multi-line value: a real failure, not a mock
	t.Setenv(bad, "prior-bad")
	t.Setenv(good, "prior-good")
	d := &dialog{out: discardWriter{}, pending: map[string]stagedKey{
		bad:  {val: badVal, prev: "prior-bad", had: true},
		good: {val: "good-value", prev: "prior-good", had: true},
	}}
	_ = os.Setenv(bad, badVal)
	_ = os.Setenv(good, "good-value")

	if ok := saveKeys(d); ok {
		t.Fatal("saveKeys = true, want false on a genuine Store failure")
	}
	if _, staged := d.pending[bad]; !staged {
		t.Error("the failed key must remain staged")
	}
	if _, staged := d.pending[good]; staged {
		t.Error("the succeeded key must be dropped from pending")
	}
	if v, _, ok := vault.Get(good); !ok || v != "good-value" {
		t.Errorf("good key not stored: %q, ok=%v", v, ok)
	}
	assertNotInVault(t, bad)
	if got := os.Getenv(good); got != "prior-good" {
		t.Errorf("succeeded key env restored = %q, want prior-good", got)
	}
	if got := os.Getenv(bad); got != badVal {
		t.Errorf("failed key env should still be shadowed by the staged value, got %q", got)
	}

	// Teeth: fix the bad value and rerun — now both succeed and pending empties out.
	d.pending[bad] = stagedKey{val: "fixed-value", prev: "prior-bad", had: true}
	if ok := saveKeys(d); !ok {
		t.Fatal("saveKeys = false after fixing the bad value, want true")
	}
	if len(d.pending) != 0 {
		t.Errorf("pending = %v, want empty after an all-success save", d.pending)
	}
}

const (
	returnKeyGood    = "CONFIGURE_RETURN_GOOD"
	returnKeyOther   = "CONFIGURE_RETURN_OTHER"
	returnKeyPrior   = "synthetic-prior"
	returnKeyValid   = "synthetic-staged"
	returnKeyInvalid = "synthetic\ninvalid"
)

// returnKeyCase is one TestRunMenuReturnKeyState scenario: the save choice, the other key's value and the outcome.
type returnKeyCase struct {
	name       string
	save       bool
	otherValue string
	wantRC     int
	failed     bool
}

func TestRunMenuReturnKeyState(t *testing.T) {
	for _, tc := range []returnKeyCase{
		{name: "all_stored", save: true, otherValue: returnKeyValid},
		{name: "discard", otherValue: returnKeyValid},
		{name: "partial_failure", save: true, otherValue: returnKeyInvalid, wantRC: 1, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, out := runMenuStagingKeys(t, tc)
			assertReturnKeyPending(t, d, out.String(), tc)
			assertReturnKeyVault(t, tc)
		})
	}
}

// runMenuStagingKeys runs the menu whose picker stages two keys and leaves, with tc's save choice on the leave
// confirmation; it checks the return code and that the picker and confirmation ran exactly once.
func runMenuStagingKeys(t *testing.T, tc returnKeyCase) (*dialog, *bytes.Buffer) {
	t.Helper()
	reg := menuTestEnv(t)
	vaulttest.Use(t)
	t.Setenv(returnKeyGood, returnKeyPrior)
	t.Setenv(returnKeyOther, "")
	if err := os.Unsetenv(returnKeyOther); err != nil {
		t.Fatal("could not unset synthetic environment variable")
	}
	originalPicker, originalConfirm := pickMenu, saveConfirmed
	t.Cleanup(func() { pickMenu, saveConfirmed = originalPicker, originalConfirm })
	var out bytes.Buffer
	d := &dialog{reg: reg, out: &out}
	calls, confirmations := 0, 0
	pickMenu = flatPick(func(string, [][2]string, int) (int, error) {
		calls++
		if !d.stage(returnKeyGood, returnKeyValid) || !d.stage(returnKeyOther, tc.otherValue) {
			t.Fatal("could not stage synthetic keys")
		}
		return -1, nil
	})
	saveConfirmed = func(string) bool {
		confirmations++
		return tc.save
	}
	if rc := runMenu(d); rc != tc.wantRC {
		t.Fatalf("return code = %d, want %d", rc, tc.wantRC)
	}
	if calls != 1 || confirmations != 1 {
		t.Fatal("expected exactly one picker call and one leave confirmation")
	}
	return d, &out
}

// assertReturnKeyPending checks which keys remain pending and what the environment and output show after the menu.
func assertReturnKeyPending(t *testing.T, d *dialog, out string, tc returnKeyCase) {
	t.Helper()
	if os.Getenv(returnKeyGood) != returnKeyPrior {
		t.Error("successful or discarded key did not restore its prior environment")
	}
	if _, pending := d.pending[returnKeyGood]; pending {
		t.Error("successful or discarded key remained pending")
	}
	if !tc.failed {
		if len(d.pending) != 0 {
			t.Error("pending should be empty after success or discard")
		}
		if _, present := os.LookupEnv(returnKeyOther); present {
			t.Error("previously absent environment variable was not removed")
		}
		return
	}
	if _, pending := d.pending[returnKeyOther]; !pending || len(d.pending) != 1 {
		t.Error("only the failed key should remain pending")
	}
	if os.Getenv(returnKeyOther) != returnKeyInvalid {
		t.Error("failed key did not retain its staged environment")
	}
	if !strings.Contains(out, returnKeyOther+": not saved:") {
		t.Error("failure diagnostic did not identify the failed key by name")
	}
}

// assertReturnKeyVault checks which keys reached the vault for tc's save choice and outcome.
func assertReturnKeyVault(t *testing.T, tc returnKeyCase) {
	t.Helper()
	names, err := vault.NamesErr()
	if err != nil {
		t.Fatal("could not inspect isolated vault names")
	}
	if slices.Contains(names, returnKeyGood) != tc.save {
		t.Error("successful key vault presence does not match save choice")
	}
	if slices.Contains(names, returnKeyOther) != (tc.save && !tc.failed) {
		t.Error("other key vault presence does not match save outcome")
	}
	if !tc.save && len(names) != 0 {
		t.Error("discard unexpectedly wrote a vault secret")
	}
}

func TestStageRejectsInvalidValueWithoutMutation(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "replacement"}[staged], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			const name = "CONFIGURE_STAGE_INVALID"
			t.Setenv(name, "original")
			d, _ := newDialog("")
			d.pending = map[string]stagedKey{}
			if staged && !d.stage(name, "already-staged") {
				t.Fatal("valid stage failed")
			}
			before, seen := d.pending[name]
			env := os.Getenv(name)
			if d.stage(name, "invalid\x00candidate") {
				t.Fatal("stage accepted a NUL value")
			}
			if got, ok := d.pending[name]; ok != seen || got != before {
				t.Errorf("pending entry changed: got %+v, present=%v; want %+v, present=%v", got, ok, before, seen)
			}
			if got := os.Getenv(name); got != env {
				t.Errorf("environment = %q, want %q", got, env)
			}
			discardKeys(d)
			if got := os.Getenv(name); got != "original" {
				t.Errorf("discard restored %q, want original", got)
			}
		})
	}
}

func TestStoreValueRejectsInvalidStagingWithoutSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const name = "CONFIGURE_STORE_INVALID"
	t.Setenv(name, "original")
	d, out := newDialog("")
	d.pending = map[string]stagedKey{}
	if storeValue(d, name, "invalid\x00candidate") {
		t.Fatal("storeValue reported success for a NUL value")
	}
	if len(d.pending) != 0 || os.Getenv(name) != "original" {
		t.Fatalf("failed store mutated pending or environment: pending=%v, env=%q", d.pending, os.Getenv(name))
	}
	if strings.Contains(out.String(), "staged ") || strings.Contains(out.String(), "Submit saves") {
		t.Errorf("failed store reported staged success: %q", out.String())
	}
}

func TestMatchPlanExactThenLongestGlob(t *testing.T) {
	plans := []providers.Plan{{ID: "*max*", Tier: "short"}, {ID: "*claude_max_20x", Tier: "long"}, {ID: "pro", Tier: "exact"}}
	cases := map[string]string{"pro": "exact", "default_claude_max_20x": "long", "max_5x": "short"}
	for id, want := range cases {
		if p, ok := matchPlan(plans, id); !ok || p.Tier != want {
			t.Errorf("matchPlan(%q) = %+v %v, want tier %q", id, p, ok, want)
		}
	}
	if _, ok := matchPlan(plans, "free"); ok {
		t.Error("an unmatched id must fall back to the computed label")
	}
	if _, ok := matchPlan([]providers.Plan{{ID: "*"}}, ""); !ok {
		t.Error(`"*" must match an agent that reports no plan id`)
	}
}

func TestModeLabelFormatsDefaultsAndFallsBack(t *testing.T) {
	reg := menuTestEnv(t)
	reg.AgentPlans = map[string][]providers.Plan{
		"antigravity": {{ID: "*", Tier: "Google AI Pro"}},
		"codex":       {{ID: "*"}},
	}
	d := &dialog{reg: reg, out: discardWriter{}}
	if got := modeLabel(d, "gemini", agentState{mode: modeSubscription}); got != "subscription (Google AI Pro)" {
		t.Errorf("antigravity label = %q", got)
	}
	if got := modeLabel(d, "codex", agentState{mode: modeSubscription, tier: providers.Subscription{Name: "Plus"}}); got != "subscription (Plus)" {
		t.Errorf("codex label (detected tier, vendor default) = %q", got)
	}
	if got := modeLabel(d, "copilot", agentState{mode: modeSubscription, tier: providers.Subscription{Name: "Pro+"}}); got != "subscription (Pro+)" {
		t.Errorf("unlisted agent = %q, want today's label", got)
	}
	reg.AgentPlanFormat = "{tier} · {provider}"
	if got := modeLabel(d, "copilot", agentState{mode: modeSubscription, tier: providers.Subscription{Name: "Pro+"}}); got != "Pro+ · GitHub" {
		t.Errorf("a set format applies to every agent, verbatim: %q", got)
	}
}

func TestWithTier(t *testing.T) {
	if got := withTier("subscription", ""); got != "subscription" {
		t.Errorf("withTier no tier = %q", got)
	}
	if got := withTier("subscription", "Max 20x"); got != "subscription (Max 20x)" {
		t.Errorf("withTier = %q", got)
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
		origPick, orig := pickMenu, saveConfirmed
		t.Cleanup(func() { saveConfirmed, pickMenu = orig, origPick })
		pickMenu = flatPick(func(string, [][2]string, int) (int, error) { // a submenu's key paste, then Submit
			storeValue(d, "GEMINI_API_KEY", "AIzaTestKey-0123456789")
			return len(actions), nil
		})
		var asked []string
		saveConfirmed = func(q string) bool { asked = append(asked, q); return yes }
		rc := runMenu(d)
		if rc != 0 {
			t.Fatalf("yes=%v: rc=%d", yes, rc)
		}
		if len(asked) != 1 || asked[0] != "Save 1 key(s) to the secrets manager" {
			t.Errorf("yes=%v: asked = %v", yes, asked)
		}
		v, src, _ := vault.Get("GEMINI_API_KEY")
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

// panelBack is the Back row tui.PickBack and tui.PickSectionsBack append to a page's rows.
var panelBack = [2]string{"Back", "return to the previous menu"}

var (
	panelSplit     = regexp.MustCompile(`\s{2,}`)
	panelBadge     = regexp.MustCompile(` (\[[^\]]*\])$`)
	numberedPrefix = regexp.MustCompile(`^  \d+\) `)
)

// panelClips lays rows out as tui's 80-column panel does and returns the cells it clipped (split copies tui.rowCells).
func panelClips(rows [][2]string) []string {
	cells := make([][]string, len(rows))
	for i, r := range rows {
		detail := strings.Join(strings.FieldsFunc(r[1], func(c rune) bool { return strings.ContainsRune("\n\r\t", c) }), " ")
		badge := ""
		if m := panelBadge.FindStringSubmatchIndex(detail); m != nil {
			badge, detail = detail[m[2]:m[3]], detail[:m[0]]
		}
		cells[i] = append([]string{r[0]}, panelSplit.Split(detail, -1)...)
		if badge != "" {
			cells[i] = append(cells[i], badge)
		}
	}
	var clipped []string
	table.Justify(cells, 78, func(c string, w int) string {
		if utf8.RuneCountInString(c) > w {
			clipped = append(clipped, c)
		}
		return table.Truncate(c, w)
	})
	return clipped
}

// assertFitsEighty fails when an 80-column panel would clip any cell of rows, which a picker lays out together.
func assertFitsEighty(t *testing.T, what string, rows [][2]string) {
	t.Helper()
	if len(rows) == 0 {
		t.Errorf("%s: no rows reached the picker", what)
	}
	if c := panelClips(rows); len(c) > 0 {
		t.Errorf("%s: an 80-column panel clips %q of rows %q", what, c, rows)
	}
}

// numberedRows parses the rows Shell's numbered prompt printed ("  N) %-22s %s") back into (name, detail) pairs.
func numberedRows(out string) [][2]string {
	var rows [][2]string
	for ln := range strings.SplitSeq(out, "\n") {
		m := numberedPrefix.FindStringIndex(ln)
		if m == nil || len(ln)-m[1] < 23 {
			continue
		}
		rest := ln[m[1]:]
		i := 22 + strings.IndexByte(rest[22:], ' ')
		rows = append(rows, [2]string{strings.TrimRight(rest[:i], " "), rest[i+1:]})
	}
	return rows
}

// The home page and every agent page, as runMenu and runAgentMenu hand them to the picker, fit 80 columns unclipped.
func TestMenuRowWidthsFitEightyColumns(t *testing.T) {
	realExtra := extraSections
	menuTestEnv(t)
	extraSections = realExtra
	d := worstCaseHomeDialog(t)
	assertFitsEighty(t, "home page", homePageRows(d))
	for name, build := range agentMenus {
		assertFitsEighty(t, name+" page", agentPageRows(d, name, build(d)))
	}
}

// worstCaseHomeDialog loads three Databricks envs (one expiring in 23h59m) and a connected tailnet with a long host.
func worstCaseHomeDialog(t *testing.T) *dialog {
	t.Helper()
	resetTailscaleStatus(t)
	tsStatusFetch = func(context.Context) (tailscale.Local, error) {
		return tailscale.Local{DNSName: "a-long-hostname-01.tail1234.ts.net."}, nil
	}
	vaulttest.Put(t, vault.Overlay, "providers", dbxDoc)
	dbxExpiry(t, "prod", time.Now().Add(23*time.Hour+59*time.Minute))
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return &dialog{reg: reg, out: discardWriter{}}
}

// homePageRows runs runMenu once and returns every row its picker lays out together, trailing Submit included.
func homePageRows(d *dialog) [][2]string {
	orig := pickMenu
	defer func() { pickMenu = orig }()
	var rows [][2]string
	pickMenu = flatPick(func(_ string, r [][2]string, _ int) (int, error) { rows = r; return -1, nil })
	runMenu(d)
	return rows
}

// agentPageRows runs runAgentMenu once and returns the rows its picker lays out, the panel's Back row included.
func agentPageRows(d *dialog, name string, m agentMenu) [][2]string {
	orig := pickAgent
	defer func() { pickAgent = orig }()
	var rows [][2]string
	pickAgent = func(_ *dialog, _ string, r [][2]string, _ int) (int, error) { rows = r; return -1, nil }
	runAgentMenu(d, name, m)
	return append(rows, panelBack)
}
