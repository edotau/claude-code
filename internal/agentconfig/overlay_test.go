package agentconfig

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// parseOverlay parses raw as a typed (non-stored) overlay document.
func parseOverlay(raw []byte) (Overlay, error) { return parseOverlayFrom(raw, "typed", false) }

// Each tampered or tricked overlay document is refused with its own message; an own 0600 file loads.
func TestLoadOverlayRefusesWritableOrTrickedFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, doc string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(doc), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // WriteFile's mode is masked by umask
			t.Fatal(err)
		}
		return p
	}
	own := write("own.json", `{"provider":"databricks","profile":"dev"}`, 0o600)
	big := `{"model":"` + strings.Repeat("a", overlayMaxBytes) + `"}`
	cases := map[string]string{
		`{"schema":3}`:              "schema 3 is newer than this build",
		`{} {}`:                     "not valid JSON at byte 4", // schemas.Decode refuses it before the trailing-data check
		big:                         "over 65536 bytes",
		`{"files":[]}`:              "files: not supported yet",
		`{"provider":"databricks"}`: "profile: required with provider",
		`{"fallback_mode":"pin"}`:   `unknown key "fallback_mode" (nearest valid key: fallback)`,
		`{"identity":"x"}`:          "identity: want auto, user, sp",
		`{"fallback":"x"}`:          "fallback_mode: ",
		`{"provider":"databricks","profile":"d\u0007ev"}`: "profile: blank or holds a control character",
	}
	if runtime.GOOS != "windows" {
		cases[write("loose.json", `{"host":"evil"}`, 0o666)] = "group- or world-writable"
		fifo := filepath.Join(dir, "fifo")
		// hosttest's PATH allowlist leaves mkfifo out; coreutils and macOS both ship it here
		if err := exec.Command("/usr/bin/mkfifo", fifo).Run(); err != nil {
			t.Fatal(err)
		}
		cases[fifo] = "not a regular file"
	}
	for doc, want := range cases {
		if _, err := LoadOverlayContext(t.Context(), doc, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadOverlayContext(t.Context(), %.40q): err = %v, want %q", doc, err, want)
		} else if strings.Contains(err.Error(), "\a") || strings.Contains(err.Error(), "evil") {
			t.Errorf("LoadOverlayContext(t.Context(), %.40q): err %q echoes a value", doc, err)
		}
	}
	if o, err := LoadOverlayContext(t.Context(), `{"fallback":"pin","provider":"databricks","profile":"dev","identity":"sp"}`, nil); err != nil ||
		o.Fallback != "pin" || o.Route().Identity != "sp" || !o.Routed() {
		t.Errorf("account/identity/pin overlay = %+v, %v", o, err)
	}
	if o, err := LoadOverlayContext(t.Context(), own, nil); err != nil || o.Confinement() != (route.Route{Provider: "databricks", Profile: "dev"}) {
		t.Errorf("own 0600 file: overlay %+v, err %v; want databricks:dev", o, err)
	}
}

// Harness-owned agent config names are refused by --env and --keep-env; an ordinary name passes.
func TestFlagOverlayRefusesHarnessOwnedNames(t *testing.T) {
	for _, name := range []string{"OPENCODE_PERMISSION", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_DIR",
		"OPENCODE_DISABLE_MODELS_FETCH", "CODEX_HOME", "COPILOT_HOME", "COPILOT_ALLOW_ALL", "COPILOT_ASSISTED_APPROVAL",
		"COPILOT_CUSTOM_INSTRUCTIONS_DIRS", "HARNESS_FOO"} {
		if _, err := FlagOverlay([]string{name + "=x"}, nil); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("--env %s: err = %v, want a refusal naming it", name, err)
		}
		if _, err := FlagOverlay(nil, []string{name}); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("--keep-env %s: err = %v, want a refusal naming it", name, err)
		}
	}
	if _, err := FlagOverlay([]string{"FOO=bar"}, []string{"BAR"}); err != nil {
		t.Errorf("FOO=bar: %v", err)
	}
}

// A @secret ref naming a profile's route credential or a harness-internal secret is refused, naming the ref.
func TestRefRuleRefusesRouteCredentialAndInternalRefs(t *testing.T) {
	setup(t)
	for doc, want := range map[string]string{
		`{"set":{"MY_PAT":"@secret:DATABRICKS_DEV_PAT"}}`: "@secret:DATABRICKS_DEV_PAT is a profile's route credential: use --profile",
		`{"set":{"A":"@secret:HARNESS_VAULT_KEY"}}`:       "@secret:HARNESS_VAULT_KEY is harness-internal",
	} {
		if _, err := LoadOverlayContext(t.Context(), doc, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadOverlayContext(t.Context(), %s): err = %v, want %q", doc, err, want)
		}
	}
	if _, err := LoadOverlayContext(t.Context(), `{"set":{"A":"@secret:MY_OWN"}}`, nil); err != nil {
		t.Errorf("an ordinary ref: %v", err)
	}
}

// An unset, keep or set entry naming a route endpoint or proxy is refused with the route-endpoint message, unechoed.
func TestOverlayUnsetKeepRefuseRouteEndpoints(t *testing.T) {
	for _, doc := range []string{`{"unset":["ANTHROPIC_BASE_URL"]}`, `{"unset":["HTTPS_PROXY"]}`,
		`{"keep":["OPENAI_BASE_URL"]}`, `{"set":{"AGY_LLM_GATEWAY_URL":"https://evil.example"}}`} {
		_, err := LoadOverlayContext(t.Context(), doc, nil)
		if !errors.Is(err, errBaseURL) {
			t.Errorf("LoadOverlayContext(t.Context(), %s): err = %v, want %q", doc, err, errBaseURL)
		} else if strings.Contains(err.Error(), "evil") {
			t.Errorf("LoadOverlayContext(t.Context(), %s): err %q echoes the value", doc, err)
		}
	}
}

// ResolveOverlay fails closed naming the set key and ref (no value, no key-tool stderr); success swaps and clones.
func TestResolveOverlayFailsClosedAndNeverEchoes(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "MY_OWN", "s3cr3t-value")
	in := func() Overlay {
		return Overlay{Set: map[string]string{"K": "@secret:MY_OWN", "PLAIN": "x"}, Unset: []string{"A"}, Keep: []string{"B"}}
	}
	o := in()
	r, err := ResolveOverlay(o)
	want := Resolved{Set: map[string]string{"K": "s3cr3t-value", "PLAIN": "x"}, Unset: []string{"A"}, Keep: []string{"B"}}
	if err != nil || !reflect.DeepEqual(r, want) {
		t.Fatalf("ResolveOverlay = %+v, %v; want %+v", r, err, want)
	}
	r.Unset[0], r.Keep[0] = "X", "X"
	if !reflect.DeepEqual(o, in()) {
		t.Errorf("ResolveOverlay shares or mutates its input: %+v", o)
	}
	cases := []struct {
		o    Overlay
		arm  func()
		want string
	}{
		{Overlay{Set: map[string]string{"K": "@secret:MISSING"}}, func() {}, "overlay set K: @secret:MISSING is not in the vault"},
		{in(), func() { // a wrong env key falls through to key_command, which fails loudly on stderr
			k, err := vault.EnsureKey()
			if err != nil {
				t.Fatal(err)
			}
			vaulttest.Keyring(t)
			vaulttest.Put(t, vault.Meta, "kid", k.Kid) // a recorded kid sends a mismatched key down the fallback chain
			vaulttest.Put(t, vault.Meta, "key_command", `["sh","-c","echo LEAKED-STDERR >&2; exit 3"]`)
			t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
		}, "overlay set K: @secret:MY_OWN: " + vault.ErrWrongKey.Error()},
	}
	if runtime.GOOS == "windows" {
		cases = cases[:1] // no sh for the failing key_command
	}
	leak := regexp.MustCompile(`LEAKED|s3cr3t`)
	for _, c := range cases {
		c.arm()
		if _, err := ResolveOverlay(c.o); err == nil || !strings.HasPrefix(err.Error(), c.want) || leak.MatchString(err.Error()) {
			t.Errorf("ResolveOverlay (%d env entries): err = %v, want prefix %q and no value or key-tool stderr", len(c.o.Set), err, c.want)
		}
	}
}

// A stored overlay applies only when the flag and process env are empty and the dispatch is not nested.
func TestDispatchOverlayPrecedence(t *testing.T) {
	vaulttest.Use(t)
	stored := `{"provider":"databricks","profile":"auto"}`
	vaulttest.Put(t, vault.Env, route.OverlayVar, stored)
	qa := `{"provider":"databricks","profile":"qa"}`
	for _, tc := range []struct{ name, flag, env, want string }{
		{"stored alone", "", "", stored},
		{"process env wins", "", `{"provider":"openai"}`, `{"provider":"openai"}`},
		{"flag wins", `{"provider":"github"}`, `{"provider":"openai"}`, `{"provider":"github"}`},
		{"nested keeps its inherited confinement, never the stored one", "", qa, qa},
	} {
		t.Setenv(route.OverlayVar, tc.env)
		if got := DispatchOverlayContext(t.Context(), tc.flag); got != tc.want {
			t.Errorf("%s: DispatchOverlay = %q, want %q", tc.name, got, tc.want)
		}
	}
	t.Setenv(route.OverlayVar, "")
	o, err := LoadOverlayContext(t.Context(), DispatchOverlayContext(t.Context(), ""), strings.NewReader(""))
	if err != nil || o.Confinement() != (route.Route{Provider: "databricks", Profile: "auto"}) {
		t.Errorf("stored overlay loaded as %+v (err %v), want confinement databricks:auto", o.Confinement(), err)
	}
}

// A run snapshot holding the overlay key empty means no overlay; an absent key or no snapshot uses the stored one.
func TestDispatchOverlayContextSnapshotEmptyKeySuppressesStored(t *testing.T) {
	vaulttest.Use(t)
	stored := `{"provider":"databricks","profile":"auto"}`
	vaulttest.Put(t, vault.Env, route.OverlayVar, stored)
	t.Setenv(route.OverlayVar, "")
	flagDoc := `{"provider":"github"}`
	for _, tc := range []struct {
		name, flag string
		ctx        context.Context
		want       string
	}{
		{"snapshot holds the key empty", "", proc.WithValues(context.Background(), proc.Values{route.OverlayVar: ""}), ""},
		{"snapshot lacks the key", "", proc.WithValues(context.Background(), proc.Values{"X": "1"}), stored},
		{"no snapshot", "", context.Background(), stored},
		{"flag wins over an empty held key", flagDoc, proc.WithValues(context.Background(), proc.Values{route.OverlayVar: ""}), flagDoc},
		{"snapshot value wins", "", proc.WithValues(context.Background(), proc.Values{route.OverlayVar: "{}"}), "{}"},
	} {
		if got := DispatchOverlayContext(tc.ctx, tc.flag); got != tc.want {
			t.Errorf("%s: DispatchOverlayContext = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTokenCommandOverlayDisableSurvivesRelay(t *testing.T) {
	p := &providers.Provider{Name: "dev", Provider: "databricks", TokenCommand: []string{"mint-bearer"}}
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev","tokenCommand":[]}`})
	o, err := LoadOverlayContext(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx = proc.WithValues(ctx, proc.Values{route.OverlayVar: string(raw)})
	resolved, err := CommandProvider(ctx, p)
	if err != nil || resolved.TokenCommand == nil || len(resolved.TokenCommand) != 0 || len(p.TokenCommand) != 1 {
		t.Fatalf("disable override lost or mutated provider: %v", err)
	}
}

func TestLoadOverlayCodexNetworkType(t *testing.T) {
	_, err := LoadOverlayContext(t.Context(), `{"schema":1,"codexNetwork":"yes"}`, nil)
	if err == nil || !strings.Contains(err.Error(), "codexNetwork: want true or false") {
		t.Errorf("bad codexNetwork: %v; want true or false", err)
	}
	o, err := LoadOverlayContext(t.Context(), `{"schema":1,"codexNetwork":true}`, nil)
	if err != nil || !o.CodexNetwork || o.Empty() || !o.Routed() {
		t.Errorf("valid codexNetwork not recognized: %v", err)
	}
}

func TestLoadOverlayCloud(t *testing.T) {
	_, err := LoadOverlayContext(t.Context(), `{"schema":1,"cloud":"yes"}`, nil)
	if err == nil || !strings.Contains(err.Error(), "cloud: want true or false") {
		t.Errorf("bad cloud: %v; want a type error naming cloud", err)
	}
	o, err := LoadOverlayContext(t.Context(), `{"schema":1,"cloud":true}`, nil)
	if err != nil || !o.Cloud || o.Empty() || !o.Routed() {
		t.Errorf("valid cloud not recognized: %v", err)
	}
}

func TestOverlayDispatchControls(t *testing.T) {
	for _, doc := range []string{`{"maxChildren":0}`, `{"maxTreeTokens":500}`, `{"budgetUSD":0.0001}`, `{"budgetCredits":30}`} {
		o, err := LoadOverlayContext(t.Context(), doc, nil)
		if err != nil || o.Empty() {
			t.Fatalf("%s: empty=%v, err=%v", doc, o.Empty(), err)
		}
	}
	for _, doc := range []string{`{"maxChildren":"bad"}`, `{"maxChildren":-1}`, `{"maxChildren":1.5}`} {
		o, err := LoadOverlayContext(t.Context(), doc, nil)
		if err != nil || o.Limit("HARNESS_DISPATCH_MAX_CHILDREN", 8) != 8 {
			t.Fatalf("%s: %v", doc, err)
		}
	}
}

func TestOverlayVendorBudgetValidation(t *testing.T) {
	for _, tc := range []struct{ doc, agent, want string }{
		{`{"budgetUSD":0.0001}`, "claude", ""},
		{`{"budgetCredits":30}`, "copilot", ""},
		{`{"budgetUSD":1}`, "codex", "claude only"},
		{`{"budgetCredits":30}`, "claude", "copilot only"},
		{`{"budgetUSD":0}`, "claude", "> 0"},
		{`{"budgetUSD":-1}`, "claude", "> 0"},
		{`{"budgetCredits":29}`, "copilot", ">= 30"},
		{`{"budgetCredits":1.5}`, "copilot", "integer"},
	} {
		o, err := LoadOverlayContext(t.Context(), tc.doc, nil)
		if err == nil {
			err = o.ValidateBudgets(tc.agent)
		}
		if tc.want == "" {
			if err != nil {
				t.Error(err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s / %s: %v; want %s", tc.doc, tc.agent, err, tc.want)
		}
	}
}

func TestOverlayProviderMode(t *testing.T) {
	if _, err := parseOverlay([]byte(`{"providerMode":{"anthropic":"exclusive"}}`)); err != nil {
		t.Errorf("valid providerMode refused: %v", err)
	}
	docs := []string{
		`{"providerMode":{"anthropic":"bogus"}}`, `{"providerMode":{"nowhere":"rotate"}}`, `{"providerMode":{"anthropic":""}}`,
	}
	for _, doc := range docs {
		if _, err := parseOverlay([]byte(doc)); err == nil || !strings.HasPrefix(err.Error(), "providerMode: ") {
			t.Errorf("%s: want a providerMode error, got %v", doc, err)
		}
	}
}

func TestLoadOverlayPresets(t *testing.T) {
	for value, want := range map[string]route.Route{
		"coworker":  {Provider: "databricks", Profile: "auto"},
		"anthropic": {Provider: "anthropic", Profile: "auto"},
	} {
		o, err := LoadOverlayContext(t.Context(), value, nil)
		if err != nil || o.Provider != want.Provider || o.Profile != want.Profile || !o.Empty() {
			t.Errorf("%s: got %+v, %v", value, o, err)
		}
	}
	for _, bad := range []string{"databricks:qa", "databricks:dev", "openai:x"} {
		_, err := LoadOverlayContext(t.Context(), bad, nil)
		if err == nil || !strings.Contains(err.Error(), "colon form is removed") {
			t.Errorf("%s: err = %v, want colon form refusal", bad, err)
		} else if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), "databricks") || strings.Contains(err.Error(), "openai") {
			t.Errorf("%s: err %q echoes the input value", bad, err)
		}
	}
	t.Chdir(t.TempDir())
	if err := os.WriteFile("coworker", []byte(`{"keep":["X"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, err := LoadOverlayContext(t.Context(), "coworker", nil); err != nil || o.Provider != "" || len(o.Keep) != 1 {
		t.Errorf("a file named like a preset must win: %+v, %v", o, err)
	}
}

func TestDerive(t *testing.T) {
	two, nine := 2, 9
	parent := Overlay{
		Model: "opus", Fallback: "none", Codemode: "on", Keep: []string{"TOKEN"}, Set: map[string]string{"A": "b"},
		MaxChildren: &two, MaxDepth: &nine, BudgetUSD: new(float64),
	}
	parent.Provider, parent.Profile, parent.Identity = "databricks", "qa", "sp"
	child, err := parent.Derive(Overlay{Model: "haiku"}, HopDispatchChild)
	if err != nil {
		t.Fatal(err)
	}
	if child.Model != "haiku" || child.Profile != "qa" || child.Identity != "sp" || child.Fallback != "none" ||
		child.Codemode != "on" || *child.MaxChildren != 2 || *child.MaxDepth != 9 ||
		child.Fence == nil || !child.Fence.Allows("qa") || child.Fence.Allows("prod") {
		t.Errorf("child lost a carried key or kept a refused one: %+v", child)
	}
	if child.Keep != nil || child.Set != nil || child.BudgetUSD != nil {
		t.Errorf("env layer or budget crossed to the child: %+v", child)
	}
	*child.MaxChildren = 7
	if *parent.MaxChildren != 2 {
		t.Error("child shares a pointer with its parent")
	}
	off := Overlay{}
	off.Provider, off.Profile = "databricks", "prod"
	if _, err := child.Derive(off, HopAppend); err == nil || !strings.Contains(err.Error(), "outside the fence") {
		t.Errorf("a profile outside the fence: err = %v", err)
	}
	for name, bad := range map[string]Overlay{"codemode": {Codemode: "loud"}, "reserved env": {Set: map[string]string{"NODE_OPTIONS": "x"}}} {
		if _, err := parent.Derive(bad, HopAppend); err == nil {
			t.Errorf("Derive accepted an invalid add (%s)", name)
		}
	}
	if _, err := parent.Derive(Overlay{}, Hop(99)); err == nil {
		t.Error("an unknown hop must refuse")
	}
	if s, _ := parent.Derive(off, HopSessionChild); s.Profile != "prod" || s.Fence != nil {
		t.Errorf("a session child has no fence and takes the typed unit: %+v", s)
	}
}

func TestDeriveResumeAndFallback(t *testing.T) {
	two := 2
	one := 1.0
	saved := Overlay{Model: "opus", Fallback: "none", MaxChildren: &two, Set: map[string]string{"A": "b"}, BudgetUSD: &one}
	saved.Provider, saved.Profile, saved.Identity = "databricks", "dev", "sp"
	got, err := saved.Derive(Overlay{}, HopResume)
	if err != nil || got.Provider != "databricks" || got.Profile != "dev" || got.Identity != "sp" || got.Model != "opus" ||
		got.Fallback != "none" || *got.MaxChildren != 2 || got.Set["A"] != "b" || got.BudgetUSD == nil {
		t.Fatalf("resume must replay the launch overlay whole: %+v, %v", got, err)
	}
	*got.MaxChildren = 9
	got.Set["A"] = "z"
	if *saved.MaxChildren != 2 || saved.Set["A"] != "b" {
		t.Error("resume child shares state with the saved overlay")
	}
	typed := Overlay{Model: "haiku"}
	typed.Provider, typed.Profile = "databricks", "qa"
	if got, err = saved.Derive(typed, HopResume); err != nil || got.Model != "haiku" || got.Profile != "qa" || got.Identity != "" {
		t.Errorf("a typed key wins on resume and the unit stays whole: %+v, %v", got, err)
	}
	fenced := saved
	fenced.Fence = &route.Fence{Profiles: []string{"dev", "qa"}}
	hop := Overlay{}
	hop.Provider, hop.Profile = "databricks", "qa"
	got, err = fenced.Derive(hop, HopFallback)
	if err != nil || got.Profile != "qa" || got.Model != "" || got.Fallback != "none" || *got.MaxChildren != 2 ||
		!got.Fence.Allows("dev") || got.Fence.Allows("prod") {
		t.Fatalf("fallback inside the fence: %+v, %v", got, err)
	}
	got.Fence.Profiles[0] = "x"
	if fenced.Fence.Profiles[0] != "dev" {
		t.Error("fallback child shares its fence with the parent")
	}
	hop.Profile = "prod"
	if _, err = fenced.Derive(hop, HopFallback); err == nil || !strings.Contains(err.Error(), "outside the fence") {
		t.Errorf("fallback outside the fence: err = %v", err)
	}
	hop.Provider, hop.Profile = "github", "dev"
	if _, err = fenced.Derive(hop, HopFallback); err == nil {
		t.Error("fallback moved the provider out of a fenced overlay")
	}
}

func TestDeriveFenceHoldsForAutoAndEmptyProfile(t *testing.T) {
	base := Overlay{Fence: &route.Fence{Profiles: []string{"qa"}}}
	for _, profile := range []string{"auto", "", "prod"} {
		add := Overlay{}
		add.Provider, add.Profile = "databricks", profile
		if _, err := base.Derive(add, HopAppend); err == nil {
			t.Errorf("profile %q passed a closed fence", profile)
		}
	}
}

func TestDeriveCapsFenceAndEnvCompose(t *testing.T) {
	two, zero := 2, 0
	base := Overlay{MaxChildren: &two, Set: map[string]string{"A": "x"}, Keep: []string{"K"}, Unset: []string{"U"}}
	got, err := base.Derive(Overlay{MaxChildren: &zero, Unset: []string{"a"}, Set: map[string]string{"u": "y"}}, HopAppend)
	if err != nil {
		t.Fatal(err)
	}
	if *got.MaxChildren != 2 {
		t.Errorf("a typed 0 (no cap) loosened the parent's cap: %d", *got.MaxChildren)
	}
	if _, set := got.Set["A"]; set || got.Set["u"] != "y" || slices.Contains(got.Unset, "U") || !slices.Contains(got.Unset, "a") {
		t.Errorf("env did not reconcile with the typed add winning: %+v", got)
	}
	if got, _ := (Overlay{Set: map[string]string{"foo": "old"}}).Derive(Overlay{Set: map[string]string{"FOO": "new"}}, HopAppend); len(got.Set) != 1 {
		t.Errorf("a case-variant Set name survived: %v", got.Set)
	}
	other := Overlay{Fence: &route.Fence{Profiles: []string{"qa"}}}
	other.Provider, other.Profile = "databricks", "qa"
	swap := Overlay{}
	swap.Provider, swap.Profile = "github", "qa"
	if _, err := other.Derive(swap, HopAppend); err == nil {
		t.Error("a fenced overlay changed provider")
	}
	if _, err := LoadOverlayContext(t.Context(), `{"provider":"databricks","profile":"prod","fence":{"profiles":["dev"]}}`, nil); err == nil {
		t.Error("a unit outside its own fence loaded")
	}
	leaf := Overlay{Fence: &route.Fence{Profiles: []string{"dev", "qa"}}}
	leaf.Provider, leaf.Profile = "databricks", "dev"
	if child, _ := leaf.Derive(Overlay{}, HopDispatchChild); !slices.Equal(child.Fence.Profiles, []string{"dev"}) {
		t.Errorf("a dispatch child's fence is its leaf's profile: %+v", child.Fence)
	}
}

// A child or fallback hop never raises a budget; a typed append or resume replaces it.
func TestDeriveBudgetNeverRaisedByChild(t *testing.T) {
	one, hundred := 1.0, 100.0
	base := Overlay{BudgetUSD: &one}
	for hop, want := range map[Hop]float64{HopAppend: 100, HopResume: 100, HopSessionChild: 1, HopDispatchChild: 1, HopFallback: 1} {
		got, err := base.Derive(Overlay{BudgetUSD: &hundred}, hop)
		if err != nil || got.BudgetUSD == nil || *got.BudgetUSD != want {
			t.Errorf("hop %d: budget %v err %v, want %v", hop, got.BudgetUSD, err, want)
		}
	}
	half := 0.5
	if got, _ := base.Derive(Overlay{BudgetUSD: &half}, HopDispatchChild); got.BudgetUSD == nil || *got.BudgetUSD != 0.5 {
		t.Errorf("a child may lower the budget: got %v", got.BudgetUSD)
	}
}

func TestDeriveCarriesEndpointWithTheUnit(t *testing.T) {
	var parent, add Overlay
	parent.Endpoint, parent.Provider, parent.Profile = "direct", "databricks", "qa"
	child, err := parent.Derive(Overlay{Model: "haiku"}, HopDispatchChild)
	if err != nil || child.Endpoint != "direct" {
		t.Fatalf("endpoint not carried: %+v %v", child, err)
	}
	add.Provider, add.Profile = "databricks", "qa"
	if o, err := parent.Derive(add, HopAppend); err != nil || o.Endpoint != "" {
		t.Errorf("a typed unit replaces the whole unit: %+v %v", o, err)
	}
	var plain Overlay
	plain.Endpoint, plain.Provider, plain.Profile = "http://example.com", "databricks", "qa"
	if _, err := parent.Derive(plain, HopAppend); err == nil {
		t.Error("a plain-http endpoint was accepted")
	}
}

func TestOverlayRunKeys(t *testing.T) {
	raw := `{"agent":"codex","tier":"fast","tools":false,"noPersist":true,"timeout":"2m","googleTools":"search, url",` +
		`"agents":{"a":{"prompt":"x"}},"systemPrompt":"p","file":"c.toml","native":true,"fleet":true}`
	o, err := parseOverlay([]byte(raw))
	if err != nil || o.Agent != "codex" || o.Tools == nil || *o.Tools || o.NoPersist == nil || !*o.NoPersist || o.Timeout != "2m" || o.Empty() {
		t.Fatalf("run keys did not decode: %+v, %v", o, err)
	}
	for _, bad := range []string{`{"tier":"huge"}`, `{"tier":""}`, `{"timeout":""}`, `{"googleTools":""}`, `{"timeout":"25h"}`, `{"timeout":"soon"}`, `{"timeout":"-1s"}`, `{"googleTools":"maps"}`,
		`{"agents":[1]}`, `{"tools":true,"noTools":true}`, `{"agent":" "}`, `{"debugFile":"a\nb"}`} {
		if _, err := parseOverlay([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := parseOverlay([]byte(`{"no-tools":true}`)); err == nil || !strings.Contains(err.Error(), "noTools") {
		t.Errorf("an unknown key must name itself and the nearest key: %v", err)
	}
}

func TestDeriveRunKeysChildren(t *testing.T) {
	parent := withRun(runKeys{Agent: "codex", Tools: new(false), NoTools: new(true), NoPersist: new(true), Native: new(true),
		Timeout: "5m", Agents: json.RawMessage(`{}`)})
	parent.Model = "opus"
	for _, hop := range []Hop{HopSessionChild, HopDispatchChild} {
		child, err := parent.Derive(withRun(runKeys{Tier: "deep", Timeout: "9m"}), hop)
		if err != nil || child.Agent != "" || child.Native != nil || child.Agents != nil || child.Tier != "deep" {
			t.Fatalf("hop %d: a non-restriction crossed or the child's own key was lost: %+v, %v", hop, child, err)
		}
		if !*child.NoTools || !*child.NoPersist || *child.Tools || child.Timeout != "5m" {
			t.Errorf("hop %d: restrictions did not carry: %+v", hop, child.runKeys)
		}
		loose, err := parent.Derive(withRun(runKeys{NoTools: new(false), NoPersist: new(false), Tools: new(false), Timeout: "1m"}), hop)
		if err != nil || !*loose.NoTools || !*loose.NoPersist || loose.Timeout != "1m" {
			t.Errorf("hop %d: a child tightens the timeout and cannot loosen the flags: %+v, %v", hop, loose.runKeys, err)
		}
		if _, err := (withRun(runKeys{NoTools: new(true)})).Derive(withRun(runKeys{Tools: new(true)}), hop); err == nil {
			t.Errorf("hop %d: tools:true over an inherited noTools was accepted", hop)
		}
		if free, err := (withRun(runKeys{Tools: new(false)})).Derive(withRun(runKeys{Tools: new(true)}), hop); err != nil || *free.Tools {
			t.Errorf("hop %d: tools:false was loosened: %+v, %v", hop, free.runKeys, err)
		}
	}
	if got, err := parent.Derive(withRun(runKeys{NoTools: new(false), NoPersist: new(false)}), HopFallback); err != nil || !*got.NoTools || !*got.NoPersist {
		t.Errorf("fallback loosened a restriction: %+v, %v", got.runKeys, err)
	}
	for _, hop := range []Hop{HopAppend, HopResume} {
		got, err := parent.Derive(withRun(runKeys{NoTools: new(false), NoPersist: new(false), Native: new(false), Timeout: "9m"}), hop)
		if err != nil || *got.NoTools || *got.NoPersist || *got.Native || got.Agent != "codex" || got.Timeout != "9m" || *got.Tools {
			t.Errorf("hop %d: typed false must override and the rest stay: %+v, %v", hop, got.runKeys, err)
		}
		*got.Tools = true
		got.Agents[0] = 'x'
		*got.Native = true
		if *parent.Tools || parent.Agents[0] != '{' || !*parent.Native {
			t.Errorf("hop %d: derived overlay shares run-key state with its base", hop)
		}
	}
}

func TestOverlayRunKeysJSON(t *testing.T) {
	if b, _ := json.Marshal(runKeys{}); string(b) != "{}" {
		t.Errorf("zero keys must all omit: %s", b)
	}
	f, tr := false, true
	all := runKeys{Agent: "a", Agents: json.RawMessage(`{}`), Tier: "fast", Tools: &f, NoTools: &f, NoPersist: &tr, DebugFile: "d",
		SystemPrompt: "s", AppendSystemPrompt: "p", OutputSchema: "o", Native: &f, Autonomous: &f, Fleet: &f, EnableMemory: &f,
		GoogleTools: "url", Timeout: "1m", File: "f"}
	want := `{"agent":"a","agents":{},"tier":"fast","tools":false,"noTools":false,"noPersist":true,"debugFile":"d","systemPrompt":"s",` +
		`"appendSystemPrompt":"p","outputSchema":"o","native":false,"autonomous":false,"fleet":false,"enableMemory":false,` +
		`"googleTools":"url","timeout":"1m","file":"f"}`
	b, err := json.Marshal(all)
	if err != nil || string(b) != want {
		t.Fatalf("order or omitempty drifted:\n%s\n%s (%v)", b, want, err)
	}
	var back runKeys
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, all) {
		t.Errorf("round trip lost a key: %+v, %v", back, err)
	}
}

func TestOverlayRunKeysSchemaAgrees(t *testing.T) {
	var doc struct {
		Properties map[string]struct {
			Enum      []string
			Pattern   string
			MinLength int
		}
	}
	if err := json.Unmarshal(OverlaySchema(), &doc); err != nil {
		t.Fatal(err)
	}
	accepts := func(key, v string) bool {
		p := doc.Properties[key]
		if len(v) < p.MinLength || (p.Enum != nil && !slices.Contains(p.Enum, v)) {
			return false
		}
		return p.Pattern == "" || regexp.MustCompile(p.Pattern).MatchString(v)
	}
	cases := []struct {
		key, v string
		ok     bool
	}{
		{"tier", "", false}, {"tier", "huge", false}, {"tier", "deep", true},
		{"timeout", "", false}, {"timeout", "soon", false}, {"timeout", "-1s", false}, {"timeout", "+5m", false}, {"timeout", "9m30s", true},
		{"googleTools", "", false}, {"googleTools", "maps", false}, {"googleTools", "search,,url", false}, {"googleTools", "search,", false},
		{"googleTools", " search", false}, {"googleTools", "search,url", true}, {"googleTools", "search, url,  code", true},
	}
	for _, c := range cases {
		_, err := parseOverlay([]byte(fmt.Sprintf(`{%q:%q}`, c.key, c.v)))
		if (err == nil) != c.ok || accepts(c.key, c.v) != c.ok {
			t.Errorf("%s=%q: want accepted=%v, Go err=%v, schema=%v", c.key, c.v, c.ok, err, accepts(c.key, c.v))
		}
	}
	if _, err := parseOverlay([]byte(`{"timeout":"24h"}`)); err != nil {
		t.Errorf("24h refused: %v", err)
	}
	if _, err := parseOverlay([]byte(`{"timeout":"24h1s"}`)); err == nil {
		t.Error("a timeout past 24h was accepted")
	}
}

func withRun(k runKeys) Overlay { return Overlay{runKeys: k} }

// An empty run value is refused whatever the key's letter case, as the schema refuses it.
func TestOverlayRunKeysEmptyAnyCase(t *testing.T) {
	for _, doc := range []string{`{"Tier":""}`, `{"TIMEOUT":""}`, `{"googletools":""}`} {
		if _, err := parseOverlay([]byte(doc)); err == nil {
			t.Errorf("%s accepted", doc)
		}
	}
}

func TestOverlayOfDoc(t *testing.T) {
	want := `{"provider":"databricks","profile":"qa","identity":"sp","host":"h","model":"a/b[1m]\"x\""}`
	if got := OverlayOf(route.Route{Provider: "databricks", Profile: "qa", Identity: "sp", Host: "h"}, `a/b[1m]"x"`).Doc(); got != want {
		t.Fatalf("got %s", got)
	}
	if got := OverlayOf(route.Route{}, "").Doc(); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Origins is the one place a key's origin is decided: a flag beats the typed document, which beats everything else.
func TestOriginsComputed(t *testing.T) {
	inherited := Overlay{Provider: "databricks", Profile: "dev", Timeout: "30s"}
	typedUnit := Overlay{Provider: "anthropic", Profile: "anthropic", Model: "sonnet"}
	for _, tc := range []struct {
		name       string
		flags      []string
		typed, eff Overlay
		key        string
		want       Origin
	}{
		{"nothing typed is inherited", nil, Overlay{}, inherited, "unit", OriginInherited},
		{"the typed document is overlay", nil, typedUnit, typedUnit, "model", OriginOverlay},
		{"any unit field is the unit key", nil, typedUnit, typedUnit, "profile", OriginOverlay},
		{"a flag key is flag", []string{"model"}, typedUnit, typedUnit, "model", OriginFlag},
		{"a flag naming a new unit", []string{"profile"}, Overlay{}, Overlay{Provider: "anthropic", Profile: "anthropic"}, "unit", OriginFlag},
		{"a flag agreeing with the inherited unit", []string{"profile"}, Overlay{}, inherited, "unit", OriginInherited},
		{"a flag agreeing with the typed unit", []string{"provider"}, typedUnit, typedUnit, "unit", OriginOverlay},
		{"a flag leaving the unit as it was", []string{"endpoint"}, Overlay{}, Overlay{Provider: "databricks", Profile: "dev"}, "unit", OriginInherited},
		{"a cleared key keeps its flag", []string{"timeout"}, Overlay{}, Overlay{}, "timeout", OriginFlag},
	} {
		if got := Origins(tc.flags, tc.typed, inherited, tc.eff).Of(tc.key); got != tc.want {
			t.Errorf("%s: origin(%s) = %v, want %v", tc.name, tc.key, got, tc.want)
		}
	}
	if !Origins([]string{"timeout"}, Overlay{}, inherited, inherited).Typed("timeout") || Origins(nil, Overlay{}, inherited, inherited).Typed("timeout") {
		t.Error("Typed: a flag key counts, an inherited one does not")
	}
}

// A document's "origins" claim is never read: a stored doc loads without it, a typed one is refused.
func TestOverlayForgedOriginsNeverRead(t *testing.T) {
	vaulttest.Use(t)
	forged := `{"provider":"anthropic","profile":"anthropic","origins":{"unit":"flag","timeout":"flag"}}`
	file := filepath.Join(t.TempDir(), "forged.json")
	if err := os.WriteFile(file, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Env, route.OverlayVar, forged)
	t.Setenv(route.OverlayVar, "")
	vaultDoc := DispatchOverlayContext(t.Context(), "")
	envCtx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: forged})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		doc  string
	}{{"env", envCtx, ""}, {"vault", t.Context(), vaultDoc}, {"file", t.Context(), file}} {
		got, err := LoadOverlayFrom(tc.ctx, tc.doc, nil, false)
		if err != nil || strings.Contains(got.Doc(), "origins") || got.Provider != "anthropic" {
			t.Errorf("%s: forged doc loaded as %s, err %v, want its keys without origins", tc.name, got.Doc(), err)
		}
	}
	if _, err := LoadOverlayFrom(t.Context(), forged, nil, true); err == nil {
		t.Error("a typed document with origins was accepted")
	}
}

// ApplyOverlay lands inherited, typed overlay and flags in that order, the unit whole, and refuses half a unit.
func TestApplyOverlay(t *testing.T) {
	reg, err := providers.Parse(assets.ProviderDefaults(), []byte(`{"providers":{"anthropic":{"work":{"protocol":"anthropic","base_url":"https://api.anthropic.com","auth":{"type":"key"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	inherited := Overlay{Model: "sonnet"}
	typed := Overlay{Provider: "anthropic", Profile: "work"}
	eff, prov, err := ApplyOverlay(reg, []string{"model"}, Overlay{Model: "opus"}, typed, inherited)
	if err != nil || eff.Model != "opus" || eff.Profile != "work" || prov.Of("model") != OriginFlag || prov.Of("profile") != OriginOverlay {
		t.Fatalf("got %+v %v %v", eff, prov, err)
	}
	bad := Overlay{Provider: "openai", Profile: "work"}
	if _, _, err := ApplyOverlay(reg, nil, Overlay{}, bad, inherited); err == nil || !strings.Contains(err.Error(), "openai") || !strings.Contains(err.Error(), "work") {
		t.Fatalf("mismatched pair: %v", err)
	}
	if _, _, err := ApplyOverlay(reg, []string{"provider"}, Overlay{Provider: "anthropic"}, typed, inherited); err == nil {
		t.Fatal("a provider without its profile must be refused")
	}
}

func TestOverlaySourceErrorsAreAtomic(t *testing.T) {
	file := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(file, []byte(`{"model":"haiku","tools":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{file, `{"model":"haiku","tools":"bad"}`, "-", `{"model":`} {
		o, err := LoadOverlayContext(t.Context(), source, nil)
		if err == nil || o.Doc() != "" || !strings.Contains(err.Error(), "overlay") {
			t.Errorf("source %q = %s, %v; want a labeled error and zero overlay", source, o.Doc(), err)
		}
	}
	if raw, err := ReadJSONSource("overlay", "-", nil); err == nil || raw != nil {
		t.Errorf("unavailable stdin = %q, %v", raw, err)
	}
	if o, err := parseOverlay([]byte(`{"model":"haiku","tools":true,"noTools":true}`)); err == nil || o.Doc() != "" {
		t.Errorf("invalid keys left a partial overlay: %s, %v", o.Doc(), err)
	}
}

func TestDeriveChildCapsKeepImplicitCeilings(t *testing.T) {
	for _, hop := range []Hop{HopSessionChild, HopDispatchChild} {
		for _, n := range []int{0, 100_000_000} {
			add := Overlay{MaxChildren: new(n), MaxTreeTokens: new(n), MaxDepth: new(n)}
			child, err := (Overlay{}).Derive(add, hop)
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range []Hop{HopAppend, HopResume, HopSessionChild, HopDispatchChild} {
				child, err = child.Derive(add, next)
				if err != nil || *child.MaxChildren != DefaultMaxDispatchChildren ||
					*child.MaxTreeTokens != DefaultMaxTreeTokens || *child.MaxDepth != DefaultMaxDispatchDepth {
					t.Fatalf("child hop %d, merge %d, n=%d: %s, %v", hop, next, n, child.Doc(), err)
				}
			}
		}
	}
	root, err := (Overlay{}).Derive(Overlay{MaxChildren: new(0)}, HopAppend)
	child, childErr := root.Derive(Overlay{}, HopDispatchChild)
	if err != nil || childErr != nil || child.MaxChildren == nil || *child.MaxChildren != 0 {
		t.Fatalf("an explicitly uncapped root must stay uncapped: %s, %v, %v", child.Doc(), err, childErr)
	}
}

func TestDeriveLoneProfileTakesTheParentProvider(t *testing.T) {
	setup(t)
	var parent Overlay
	parent.Endpoint, parent.Provider, parent.Profile = "direct", "databricks", "dev"
	lone, err := parseOverlayFrom([]byte(`{"profile":"dev2"}`), "typed", false)
	if err != nil {
		t.Fatalf("a lone profile must parse: %v", err)
	}
	got, err := parent.Derive(lone, HopAppend)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "databricks" || got.Profile != "dev2" || got.Endpoint != "direct" {
		t.Fatalf("child did not inherit the parent's provider and endpoint: %+v", got)
	}
	var nosuch Overlay
	nosuch.Profile = "nosuch"
	for name, c := range map[string]struct{ parent, child Overlay }{
		"a parent whose provider lacks the profile": {parent, nosuch},
		"no parent unit": {Overlay{}, lone},
	} {
		if _, err := c.parent.Derive(c.child, HopAppend); err == nil || !strings.Contains(err.Error(), "provider: required") {
			t.Errorf("%s: err = %v, want provider: required", name, err)
		}
	}
	if lone.Provider != "" {
		t.Error("Derive changed its argument")
	}
}
