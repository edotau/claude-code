package providers

import (
	"bytes"
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func selectTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := load(t)
	t.Setenv(EnvSessionProfile, "")
	return reg
}

func writeProviderPins(t *testing.T, pins map[string]string) {
	t.Helper()
	if err := vault.Update(func(tx *vault.Tx) error {
		for key := range tx.All(vault.Pins) {
			if err := tx.Put(vault.Pins, key, ""); err != nil {
				return err
			}
		}
		for key, value := range pins {
			if err := tx.Put(vault.Pins, key, value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSelectReadsVaultPinsWithSlotPrecedence(t *testing.T) {
	reg := selectTestRegistry(t)
	vaulttest.SetRecord(t, "claude", "profile", "gateway")
	vaulttest.SetRecord(t, "claude", "model", "all-from-vault")
	vaulttest.SetRecord(t, "claude", "aliases.opus", "openai:gpt-5-nano")
	vaulttest.SetRecord(t, "claude", "aliases.haiku", "haiku-from-vault")

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gateway" {
		t.Fatalf("provider = %q, want gateway from vault pins", sel.Provider.Name)
	}
	for _, alias := range models.AliasNames {
		want := Target{Provider: sel.Provider, Model: "all-from-vault"}
		switch alias {
		case models.Opus:
			want = Target{Provider: reg.Providers["openai"], Model: "gpt-5-nano"}
		case models.Haiku:
			want.Model = "haiku-from-vault"
		}
		got, ok := sel.Aliases[alias]
		if !ok || got.Provider != want.Provider || got.Model != want.Model {
			t.Errorf("alias %s = %+v, present %v; want %+v", alias, got, ok, want)
		}
	}
}

func TestSelectIgnoresEnvAndFlagBeatsPin(t *testing.T) {
	reg := selectTestRegistry(t)
	vaulttest.SetRecord(t, "claude", "profile", "gateway")
	vaulttest.SetRecord(t, "claude", "model", "all-from-vault")
	vaulttest.SetRecord(t, "claude", "aliases.opus", "opus-from-vault")
	t.Setenv(EnvSessionProfile, "  gw  ")
	t.Setenv("CLAUDE_MODEL", "  all-from-env  ")
	t.Setenv("CLAUDE_OPUS_MODEL", "env-opus")

	checkSelectIgnoresEnv(t, reg)

	t.Setenv(EnvSessionProfile, "")
	vaulttest.SetRecord(t, "claude", "profile", "gw")
	vaulttest.SetRecord(t, "claude", "model", "")
	vaulttest.SetRecord(t, "claude", "aliases.opus", "opus-from-vault")
	checkFlagBeatsPin(t, reg)
}

func checkSelectIgnoresEnv(t *testing.T, reg *Registry) {
	t.Helper()
	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gateway" {
		t.Fatalf("provider = %q, want the vault pin gateway (pins are vault-only)", sel.Provider.Name)
	}
	if got := sel.Aliases[models.Opus]; got.Provider != sel.Provider || got.Model != "opus-from-vault" {
		t.Errorf("opus = %+v, want the vault value, not the env", got)
	}
	if got := sel.Aliases[models.Sonnet]; got.Provider != sel.Provider || got.Model != "all-from-vault" {
		t.Errorf("sonnet = %+v, want the vault all-model pin, not the env", got)
	}
}

func checkFlagBeatsPin(t *testing.T, reg *Registry) {
	t.Helper()
	flagSel, err := Select(reg, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if flagSel.Provider.Name != "gateway" {
		t.Errorf("provider flag selected %q, want gateway over vault pin gw", flagSel.Provider.Name)
	}
	if got := flagSel.Aliases[models.Opus]; got.Provider != flagSel.Provider || got.Model != "opus-from-vault" {
		t.Errorf("flag-selected opus = %+v, want vault value on gateway", got)
	}
}

func TestSelectAllModelPinBeatsProviderDefaults(t *testing.T) {
	reg := selectTestRegistry(t)
	vaulttest.SetRecord(t, "claude", "profile", "gateway")
	vaulttest.SetRecord(t, "claude", "model", "all-from-vault")

	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range models.AliasNames {
		if got := sel.Aliases[alias]; got.Provider != sel.Provider || got.Model != "all-from-vault" {
			t.Errorf("alias %s = %+v, want all-model pin", alias, got)
		}
	}

	vaulttest.SetRecord(t, "claude", "model", "")
	sel, err = Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range models.AliasNames {
		checkProviderDefault(t, sel, alias)
	}
}

// checkProviderDefault asserts alias resolves to the provider's own default model, or to nothing when it has none.
func checkProviderDefault(t *testing.T, sel Selection, alias string) {
	t.Helper()
	want := sel.Provider.Models[alias]
	got, ok := sel.Aliases[alias]
	if want == "" {
		if ok {
			t.Errorf("alias %s = %+v, want no provider default", alias, got)
		}
		return
	}
	if !ok || got.Provider != sel.Provider || got.Model != want {
		t.Errorf("alias %s = %+v, want provider default %q", alias, got, want)
	}
}

// EffectiveSecret must try env, then the vault, in that order, and miss cleanly.
func TestEffectiveSecretOrder(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("ORDER_NAME", "")
	vaulttest.Secret(t, "ORDER_NAME", "from-vault")

	if v, src, err := vault.EffectiveSecret("ORDER_NAME"); err != nil || v != "from-vault" || src != "vault" {
		t.Errorf("EffectiveSecret (vault) = (%q, %q, %v), want (from-vault, vault, nil)", v, src, err)
	}

	t.Setenv("ORDER_NAME", "  from-env\n")
	if v, src, err := vault.EffectiveSecret("ORDER_NAME"); err != nil || v != "from-env" || src != "env" {
		t.Errorf("EffectiveSecret (env beats vault) = (%q, %q, %v), want (from-env, env, nil)", v, src, err)
	}
}

// A whitespace-only env var must be treated as unset, not as a winning value.
func TestEffectiveSecretIgnoresWhitespaceOnlyEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "WS_NAME", "from-vault")
	t.Setenv("WS_NAME", "   ")

	if v, src, err := vault.EffectiveSecret("WS_NAME"); err != nil || v != "from-vault" || src != "vault" {
		t.Errorf("EffectiveSecret with whitespace-only env = (%q, %q, %v), want (from-vault, vault, nil)", v, src, err)
	}
	if got := HasSecret("WS_NAME"); got != "vault" {
		t.Errorf("HasSecret with whitespace-only env = %q, want vault", got)
	}
}

// A name held nowhere must miss cleanly: ("", "", nil), and HasSecret reports "".
func TestEffectiveSecretMiss(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("NEVER_SET_ANYWHERE", "")

	if v, src, err := vault.EffectiveSecret("NEVER_SET_ANYWHERE"); v != "" || src != "" || err != nil {
		t.Errorf("EffectiveSecret(miss) = (%q, %q, %v), want (\"\", \"\", nil)", v, src, err)
	}
	if got := HasSecret("NEVER_SET_ANYWHERE"); got != "" {
		t.Errorf("HasSecret(miss) = %q, want \"\"", got)
	}
}

// A vault record that cannot be opened surfaces as EffectiveSecret's error; StoredSecret reads it as a miss.
func TestEffectiveSecretLockedVaultErrors(t *testing.T) {
	lockVault(t, "LOCKED_NAME", "sealed")
	t.Setenv("LOCKED_NAME", "")
	if v, src, err := vault.EffectiveSecret("LOCKED_NAME"); err == nil || v != "" || src != "" {
		t.Errorf("EffectiveSecret(locked) = (%q, %q, %v), want an error and no value", v, src, err)
	}
	if got := StoredSecret("LOCKED_NAME"); got != "" {
		t.Errorf("StoredSecret(locked) = %q, want \"\"", got)
	}
}

// HasSecret reports env|vault|"" from presence alone: it never resolves a key, so a wrong key, a
// down keyring and no keyring at all change nothing.
func TestHasSecretNeverResolvesAKey(t *testing.T) {
	lockVault(t, "HAS_NAME", "sealed")
	t.Setenv("HAS_NAME", "")
	t.Setenv("HAS_NOT", "")
	kr := vaulttest.Keyring(t)
	kr.Down = true
	if got := HasSecret("HAS_NAME"); got != "vault" {
		t.Errorf("HasSecret(sealed, wrong key, keyring down) = %q, want vault", got)
	}
	t.Setenv("HARNESS_VAULT_KEY", "")
	if got := HasSecret("HAS_NAME"); got != "vault" {
		t.Errorf("HasSecret(sealed, no key) = %q, want vault", got)
	}
	if got := HasSecret("HAS_NOT"); got != "" {
		t.Errorf("HasSecret(absent) = %q, want \"\"", got)
	}
	t.Setenv("HAS_NAME", "from-env")
	if got := HasSecret("HAS_NAME"); got != "env" {
		t.Errorf("HasSecret(env set) = %q, want env", got)
	}
}

// CopilotModel resolves the COPILOT_MODEL pin first; with no pin, it falls back to the
// github-copilot provider's own opus alias — and names which source won in both cases.
func TestCopilotModelPinBeatsFallback(t *testing.T) {
	reg := load(t)
	vaulttest.SetRecord(t, "copilot", "model", "")

	wantFallback := reg.Providers["personal"].Models[models.Opus]
	if got, src := CopilotModel(reg); got != wantFallback || src != "github-copilot default" {
		t.Errorf("CopilotModel(unset) = (%q, %q), want (%q, github-copilot default)", got, src, wantFallback)
	}

	vaulttest.SetRecord(t, "copilot", "model", "claude-opus-5.5")
	if got, src := CopilotModel(reg); got != "claude-opus-5.5" || src != "pin" {
		t.Errorf("CopilotModel(pinned) = (%q, %q), want (claude-opus-5.5, pin)", got, src)
	}
}

// A process env var for COPILOT_MODEL is ignored: pins are vault-only.
func TestCopilotModelIgnoresEnv(t *testing.T) {
	reg := load(t)
	vaulttest.SetRecord(t, "copilot", "model", "from-vault")
	if got, src := CopilotModel(reg); got != "from-vault" || src != "pin" {
		t.Errorf("CopilotModel(env set) = (%q, %q), want (from-vault, pin): env is not a pin source", got, src)
	}
}

// CodexModel resolves CODEX_MODEL: the pin if set, else "" (codex's own default decides, not the registry).
func TestCodexModelResolverPinVsDefault(t *testing.T) {
	load(t)
	vaulttest.SetRecord(t, "codex", "model", "")
	if got, src := CodexModel(); got != "" || src != "default" {
		t.Errorf("CodexModel(unset) = (%q, %q), want (\"\", default)", got, src)
	}
	vaulttest.SetRecord(t, "codex", "model", "gpt-6.1-sol")
	if got, src := CodexModel(); got != "gpt-6.1-sol" || src != "pin" {
		t.Errorf("CodexModel(pinned) = (%q, %q), want (gpt-6.1-sol, pin)", got, src)
	}
}

// CodexEffort resolves CODEX_EFFORT the same way; a process env var is not a pin source.
func TestCodexEffortResolverPinVsDefault(t *testing.T) {
	load(t)
	vaulttest.SetRecord(t, "codex", "effort", "")
	if got, src := CodexEffort(); got != "" || src != "default" {
		t.Errorf("CodexEffort(unset) = (%q, %q), want (\"\", default)", got, src)
	}
	vaulttest.SetRecord(t, "codex", "effort", "high")
	if got, src := CodexEffort(); got != "high" || src != "pin" {
		t.Errorf("CodexEffort(pinned) = (%q, %q), want (high, pin)", got, src)
	}
	t.Setenv("CODEX_EFFORT", "low")
	if got, src := CodexEffort(); got != "high" || src != "pin" {
		t.Errorf("CodexEffort(env set) = (%q, %q), want (high, pin): env is not a pin source", got, src)
	}
}

// GeminiModel resolves GEMINI_MODEL from the vault only: the pin if set, else "" (agy's own default decides).
func TestGeminiModelResolverPinVsDefaultIgnoresEnv(t *testing.T) {
	load(t)
	vaulttest.SetRecord(t, "gemini", "model", "")
	if got, src := GeminiModel(); got != "" || src != "default" {
		t.Errorf("GeminiModel(unset) = (%q, %q), want (\"\", default): env must not leak in", got, src)
	}
	vaulttest.SetRecord(t, "gemini", "model", "gemini-3.8-flash-high")
	if got, src := GeminiModel(); got != "gemini-3.8-flash-high" || src != "pin" {
		t.Errorf("GeminiModel(pinned) = (%q, %q), want (gemini-3.8-flash-high, pin)", got, src)
	}
}

// TestSelectDoesNotTranslateLegacyDatabricksPin: a databricks-<env>[-oai] pin is taken as written, never mapped to <env>.
func TestSelectDoesNotTranslateLegacyDatabricksPin(t *testing.T) {
	reg := selectTestRegistry(t)
	row := *reg.Providers[reg.Default] // a migrated <env> preset row the old pin used to be rewritten to
	row.Name, row.Preset = "qa", PresetDatabricks
	reg.Providers["qa"] = &row
	for _, pin := range []string{"databricks-qa", "databricks-dev-oai"} {
		vaulttest.SetRecord(t, "claude", "profile", pin)
		sel, err := Select(reg, "")
		if err == nil || !strings.Contains(err.Error(), pin) {
			t.Errorf("Select with pin %q = %+v, %v; want an unknown-provider error naming the pin", pin, sel.Provider, err)
		}
	}
}

const ghRow = `"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},` +
	`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"}`

// copilotLoginReg is a hermetic vault with github profiles personal (login=tester) and bare (no login).
func copilotLoginReg(t *testing.T, pin string) *Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{`+
		`"personal":{`+ghRow+`,"login":"tester"},"bare":{`+ghRow+`}}}}`)
	if pin != "" {
		vaulttest.SetRecord(t, "copilot", "profile", pin)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestCopilotLogin(t *testing.T) {
	for _, tc := range []struct{ name, pin, login, profile string }{
		{"profile pin resolves to its login", "personal", "tester", "personal"},
		{"unset pin is gh's active account", "", "", ""},
		{"a pin naming no github profile is gh's active account", "octocat", "", ""},
		{"profile without a login passes none", "bare", "", "bare"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login, profile := CopilotLogin(copilotLoginReg(t, tc.pin))
			if login != tc.login || profile != tc.profile {
				t.Errorf("CopilotLogin = (%q, %q), want (%q, %q)", login, profile, tc.login, tc.profile)
			}
		})
	}
}

func TestGithubAccountNameAndLoginRoundTrip(t *testing.T) {
	reg := copilotLoginReg(t, "")
	for _, tc := range []struct{ in, name string }{
		{"tester", "personal"}, // login -> profile
		{"octocat", "octocat"}, // unmigrated login stays raw
		{"", ""},
	} {
		if got := GithubAccountName(reg, tc.in); got != tc.name {
			t.Errorf("GithubAccountName(%q) = %q, want %q", tc.in, got, tc.name)
		}
	}
	for _, tc := range []struct{ in, login string }{
		{"personal", "tester"}, {"bare", ""}, {"octocat", "octocat"},
	} {
		if got := GithubAccountLogin(reg, tc.in); got != tc.login {
			t.Errorf("GithubAccountLogin(%q) = %q, want %q", tc.in, got, tc.login)
		}
	}
	if got := GithubAccountLogin(reg, GithubAccountName(reg, "tester")); got != "tester" {
		t.Errorf("round trip = %q, want tester", got)
	}
	if GithubAccountName(nil, "tester") != "tester" || GithubAccountLogin(nil, "x") != "x" {
		t.Error("nil registry must pass the value through")
	}
}

// AgentRecord(s) read what a vault write stored; an unset agent is the zero record.
func TestAgentRecordsReadStoredRecords(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Update(func(tx *vault.Tx) error {
		return tx.PutAgentRecords(map[string]vault.AgentRecord{"codex": {Profile: "chatgpt", Model: "m1"}})
	}); err != nil {
		t.Fatal(err)
	}
	if got := AgentRecord("codex"); got.Profile != "chatgpt" || got.Model != "m1" {
		t.Errorf("AgentRecord(codex) = %+v", got)
	}
	if got := AgentRecord("gemini"); !reflect.DeepEqual(got, vault.AgentRecord{}) {
		t.Errorf("AgentRecord(gemini) = %+v, want zero", got)
	}
	if recs := AgentRecords(); len(recs) != 1 || recs["codex"].Profile != "chatgpt" {
		t.Errorf("AgentRecords = %+v, want only codex", recs)
	}
}

func orderDoc(names ...string) []byte {
	var rows strings.Builder
	for i, n := range names {
		if i > 0 {
			rows.WriteString(",")
		}
		rows.WriteString(`"` + n + `":{"preset":"databricks","host":"https://` + n + `.cloud.databricks.com"}`)
	}
	return []byte(`{"providers":{"databricks":{` + rows.String() + `}}}`)
}

func TestProfilesOfReorderedDocReordersOutput(t *testing.T) {
	vaulttest.UseKeyring(t)
	reg, err := Parse(orderDoc("dev", "qa", "prod"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reg.ProfilesOf("databricks"), []string{"dev", "qa", "prod"}; !slices.Equal(got, want) {
		t.Errorf("ProfilesOf = %v, want %v", got, want)
	}
	if got, want := DatabricksProfiles(reg), []string{"dev", "qa", "prod"}; !slices.Equal(got, want) {
		t.Errorf("DatabricksProfiles = %v, want %v", got, want)
	}
	// negative control: the same rows declared in another order come out in that order, not sorted
	re, err := Parse(orderDoc("prod", "dev", "qa"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := re.ProfilesOf("databricks"), []string{"prod", "dev", "qa"}; !slices.Equal(got, want) {
		t.Errorf("reordered ProfilesOf = %v, want %v", got, want)
	}
}

func TestProfilesOfOverlayRowsFollowDefaults(t *testing.T) {
	vaulttest.UseKeyring(t)
	reg, err := Parse(orderDoc("qa", "dev"), orderDoc("zeta", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reg.ProfilesOf("databricks"), []string{"qa", "dev", "zeta", "alpha"}; !slices.Equal(got, want) {
		t.Errorf("ProfilesOf = %v, want defaults then overlay rows in declared order %v", got, want)
	}
}

func TestPinIgnoresProcessEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	if got := AgentRecord("copilot").Model; got != "" {
		t.Errorf("Pin(env only) = %q, want empty: pins are vault-only", got)
	}
	vaulttest.SetRecord(t, "copilot", "model", "from-vault")
	if got := AgentRecord("copilot").Model; got != "from-vault" {
		t.Errorf("Pin(vault+env) = %q, want from-vault", got)
	}
}

func TestPinProfileIsNotTheSessionMarker(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv(EnvSessionProfile, "qa")
	if got := AgentRecord("claude").Profile; got != "" {
		t.Errorf("claude record profile = %q, want empty: the session marker is read by SessionProfile only", got)
	}
	if got := SessionProfile(); got != "qa" {
		t.Errorf("SessionProfile = %q, want qa", got)
	}
}

func TestValidRouteAccountKey(t *testing.T) {
	for key, want := range map[string]bool{
		"anthropic·ultra": true, "github·copilot": true, "github·someuser": true, "opencode·go": true,
		"copilot:x": false, "copilot": false, "anthropic:ultra": false, "": false,
	} {
		if got := validRouteAccountKey(key); got != want {
			t.Errorf("validRouteAccountKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestRenderPinsSorted(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Pins, "B", "2")
	vaulttest.Put(t, vault.Pins, "A", "1")
	got, err := RenderPins()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("# Managed by `claude-code switch` / `claude-code models --pin`.\nA=1\nB=2\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("RenderPins() = %q, want %q", got, want)
	}
}

func TestRenderPinsAbsent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, existing := range []bool{false, true} {
		if existing {
			vaulttest.Put(t, vault.Env, "PLAIN", "value")
		}
		if _, err := RenderPins(); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("RenderPins (existing vault %v) error = %v, want fs.ErrNotExist", existing, err)
		}
	}
}

func TestSelectSurfacesUnreadableVault(t *testing.T) {
	reg := selectTestRegistry(t)
	vaulttest.Put(t, vault.Meta, "schema", "999")
	for _, env := range []string{"", "openai"} {
		t.Setenv(EnvSessionProfile, env)
		if _, err := Select(reg, ""); err == nil || !strings.Contains(err.Error(), "vault: pins unreadable") {
			t.Errorf("Select (env %q) error = %v, want vault: pins unreadable", env, err)
		}
	}
	if got := SessionProfile(); got != "openai" {
		t.Errorf("SessionProfile() = %q, want env value openai despite unreadable vault", got)
	}
}

func TestSetProviderModelConcurrentSlots(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	start := make(chan struct{})
	results := make(chan error, 2)
	for alias, model := range map[string]string{"opus": "a", "haiku": "b"} {
		go func() {
			<-start
			results <- SetProviderModel("openai", alias, model)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Errorf("SetProviderModel: %v", err)
		}
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers["openai"]
	if p.Models["opus"] != "a" || p.Models["haiku"] != "b" {
		t.Fatalf("concurrent model edits lost an alias: %v, want opus=a and haiku=b", p.Models)
	}
}

func TestSetProviderFieldsPreservesPreviousDoc(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderFields("openai", map[string]any{"base_url": "https://first.example/v1"}); err != nil {
		t.Fatal(err)
	}
	first, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetProviderFields("openai", map[string]any{"base_url": "https://second.example/v1"}); err != nil {
		t.Fatal(err)
	}
	checkPrevOverlay(t, first)
	current, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(current, first) {
		t.Error("second write did not update overlay/providers")
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Providers["openai"].BaseURL; got != "https://second.example/v1" {
		t.Errorf("second base_url = %q", got)
	}
}

// checkPrevOverlay asserts overlay/providers.prev holds the doc written before the latest write.
func checkPrevOverlay(t *testing.T, first []byte) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		prev, ok := tx.Get(vault.Overlay, "providers.prev")
		if !ok || !bytes.Equal([]byte(prev), first) {
			t.Errorf("providers.prev = %q, present %v; want %q", prev, ok, first)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSetProviderFieldsRefusesInvalidOverlay(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const prior = `{"providers":{"openai":{"openai":{"protocol":"openai","base_url":"https://example.com/v1","auth":{"type":"none"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", prior)
	vaulttest.Put(t, vault.Overlay, "providers.prev", "{}")
	if err := Validate([]byte(prior)); err != nil {
		t.Fatalf("valid initial overlay: %v", err)
	}
	if err := SetProviderFields("openai", map[string]any{"base_url": ""}); err == nil {
		t.Fatal("empty base_url with no routes must fail validation")
	}
	if err := vault.View(func(tx *vault.Tx) error {
		for key, want := range map[string]string{"providers": prior, "providers.prev": "{}"} {
			if got, ok := tx.Get(vault.Overlay, key); !ok || got != want {
				t.Errorf("refused edit changed %s: %q, present %v; want %q", key, got, ok, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestLoadMalformedVaultOverlay(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "vault overlay/providers") {
		t.Fatalf("Load() error = %v, want vault overlay/providers", err)
	}
}

func TestGitHubAuthLogin(t *testing.T) {
	for _, tc := range []struct{ name, pin, profile, login string }{
		{"profile pin resolves to its login", "personal", "personal", "tester"},
		{"unset pin is gh's active account", "", "", ""},
		{"profile without a login reports none, not a bare pin", "bare", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := copilotLoginReg(t, "")
			if tc.pin != "" {
				if err := vault.SetGitHubAuth(tc.pin); err != nil {
					t.Fatal(err)
				}
			}
			profile, login := GitHubAuthLogin(reg)
			if profile != tc.profile || login != tc.login {
				t.Errorf("GitHubAuthLogin = (%q, %q), want (%q, %q)", profile, login, tc.profile, tc.login)
			}
		})
	}
}

func TestCopilotLoginIgnoresGitHubAuthPin(t *testing.T) {
	reg := copilotLoginReg(t, "")
	if err := vault.SetGitHubAuth("personal"); err != nil {
		t.Fatal(err)
	}
	if login, profile := CopilotLogin(reg); login != "" || profile != "" {
		t.Errorf("CopilotLogin = (%q, %q), want (\"\", \"\"): github-auth never feeds copilot", login, profile)
	}
}
