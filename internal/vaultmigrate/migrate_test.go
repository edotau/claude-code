package vaultmigrate

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const flatOverlay = `{"fallback":["subscription"],"providers":{
 "dev":{"auth":{},"cliProfile":"dev","host":"https://dbc-1.cloud.databricks.com","preset":"databricks"},
 "qa":{"auth":{"authType":"u2m"},"cliProfile":"qa","host":"https://dbc-2.cloud.databricks.com","preset":"databricks"},
 "prod":{"auth":{"authType":"u2m"},"cliProfile":"prod","host":"https://dbc-3.cloud.databricks.com","preset":"databricks"},
 "digitalbtd-digitalbtd-user-qa":{"auth":{"authType":"oauth-m2m","clientId":"cid"},"cliProfile":"m2m","host":"https://dbc-4.cloud.databricks.com","preset":"databricks"},
 "github-copilot":{"auth":{"type":"passthrough","login":"copilot"},"base_url":"https://api.githubcopilot.com","kind":"openai",
   "models":{"haiku":"claude-haiku-5.5","opus":"gpt-6-astra","sonnet":"kimi-k3"},"opencodeID":"github-copilot"},
 "ollama":null}}`

var secrets = map[string]string{
	"PROVIDER_QA_REFRESH_TOKEN": "refresh-qa", "PROVIDER_QA_ACCESS_TOKEN": "access-qa",
	"PROVIDER_DIGITALBTD_DIGITALBTD_USER_QA_CLIENT_SECRET": "m2m-secret",
	"ANTHROPIC_API_KEY": "sk-ant-x", "TAVILY_API_KEY": "tvly-x", "CLAUDE_CODE_OAUTH_TOKEN": "setup-tok",
	"PROVIDER_GONE_PAT": "orphan",
}

var pins = map[string]string{
	"HARNESS_PROVIDER": "dev", "COPILOT_GITHUB_USER": "octocat", "GEMINI_AGENT_MODEL": "m1",
	"CODEX_REASONING_EFFORT": "high", "HARNESS_OPUS_MODEL": "subscription:claude-opus-5-5", "CODEX_PROFILE": "gemini",
}

func fixture(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error {
		for n, v := range secrets {
			if err := tx.SealSecret(k, n, v, vault.Record{}); err != nil {
				return err
			}
		}
		for n, v := range pins {
			if err := tx.Put(vault.Pins, n, v); err != nil {
				return err
			}
		}
		if err := tx.Put(vault.Env, "TAVILY_API_KEY", "@secret"); err != nil {
			return err
		}
		if err := tx.Put(vault.Overlay, "providers", flatOverlay); err != nil {
			return err
		}
		return tx.SetSchema(2)
	}); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, name string) (string, bool) {
	t.Helper()
	v, ok, err := vault.ReadSecret(name)
	if err != nil {
		t.Fatal(err)
	}
	return v, ok
}

func TestPlanListsExactRenames(t *testing.T) {
	fixture(t)
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compute(reg)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range plan.Changes {
		got = append(got, string(c.Kind)+" "+c.Old+" -> "+c.New)
	}
	want := []string{
		"pin-value CODEX_PROFILE=gemini -> CODEX_PROFILE=antigravity",
		"pin-key CODEX_REASONING_EFFORT -> CODEX_EFFORT",
		"pin-key COPILOT_GITHUB_USER -> COPILOT_PROFILE",
		"pin-key GEMINI_AGENT_MODEL -> GEMINI_MODEL",
		"pin-key HARNESS_OPUS_MODEL -> CLAUDE_OPUS_MODEL",
		"pin-key HARNESS_PROVIDER -> CLAUDE_PROFILE",
		"secret ANTHROPIC_API_KEY -> ANTHROPIC_KEY",
		"secret PROVIDER_DIGITALBTD_DIGITALBTD_USER_QA_CLIENT_SECRET -> DATABRICKS_DIGITALBTD_DIGITALBTD_USER_QA_M2M",
		"secret PROVIDER_QA_REFRESH_TOKEN + PROVIDER_QA_ACCESS_TOKEN -> DATABRICKS_QA_OAUTH",
		"overlay overlay/providers -> overlay/providers",
		"schema schema 2 -> schema 3",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var sk []string
	for _, c := range plan.Skipped {
		sk = append(sk, c.Old)
	}
	if want := []string{"CLAUDE_CODE_OAUTH_TOKEN", "PROVIDER_GONE_PAT"}; !slices.Equal(sk, want) {
		t.Fatalf("skipped = %v, want %v", sk, want)
	}
	var b strings.Builder
	plan.Render(&b)
	for _, v := range secrets {
		if strings.Contains(b.String(), v) {
			t.Fatalf("table leaks a secret value %q:\n%s", v, b.String())
		}
	}
}

func TestApplyMigratesAndIsIdempotent(t *testing.T) {
	fixture(t)
	reg, _ := Current()
	res, err := Apply(reg)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(res.Backup)
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("backup %q: %v mode %v", res.Backup, err, fi)
	}
	// secrets re-sealed under the new names, old names gone
	for old, new := range map[string]string{
		"ANTHROPIC_API_KEY": "ANTHROPIC_KEY",
		"PROVIDER_DIGITALBTD_DIGITALBTD_USER_QA_CLIENT_SECRET": "DATABRICKS_DIGITALBTD_DIGITALBTD_USER_QA_M2M",
	} {
		if v, ok := read(t, new); !ok || v != secrets[old] {
			t.Errorf("%s = %q,%v want %q", new, v, ok, secrets[old])
		}
		if _, ok := read(t, old); ok {
			t.Errorf("old name %s still present", old)
		}
	}
	if v, _ := read(t, "DATABRICKS_QA_OAUTH"); v != `{"refresh":"refresh-qa","access":"access-qa"}` {
		t.Errorf("oauth value = %q", v)
	}
	for _, old := range []string{"PROVIDER_QA_REFRESH_TOKEN", "PROVIDER_QA_ACCESS_TOKEN"} {
		if _, ok := read(t, old); ok {
			t.Errorf("old name %s still present", old)
		}
	}
	for _, same := range []string{"TAVILY_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "PROVIDER_GONE_PAT"} {
		if v, ok := read(t, same); !ok || v != secrets[same] {
			t.Errorf("untouched %s = %q,%v", same, v, ok)
		}
	}
	// pins
	_ = vault.View(func(tx *vault.Tx) error {
		got := tx.All(vault.Pins)
		want := map[string]string{"CLAUDE_PROFILE": "dev", "COPILOT_PROFILE": "personal", "GEMINI_MODEL": "m1",
			"CODEX_EFFORT": "high", "CLAUDE_OPUS_MODEL": "max:claude-opus-5-5", "CODEX_PROFILE": "antigravity"}
		if len(got) != len(want) {
			t.Errorf("pins = %v", got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("pin %s = %q want %q", k, got[k], v)
			}
		}
		if tx.Schema() != 3 {
			t.Errorf("schema = %d", tx.Schema())
		}
		return nil
	})
	// backup opens with the old content
	_ = vault.ViewAt(res.Backup, func(tx *vault.Tx) error {
		if _, ok := tx.Get(vault.Pins, "HARNESS_PROVIDER"); !ok || tx.Schema() != 2 {
			t.Errorf("backup lost old content (schema %d)", tx.Schema())
		}
		if _, ok := tx.Get(vault.Secrets, "ANTHROPIC_API_KEY"); !ok {
			t.Error("backup lacks old secret")
		}
		return nil
	})
	// second run is a no-op
	reg2, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := Compute(reg2)
	if err != nil || !plan2.Empty() {
		t.Fatalf("second plan = %+v, %v", plan2.Changes, err)
	}
	res2, err := Apply(reg2)
	if err != nil || res2.Backup != "" {
		t.Fatalf("second apply = %+v, %v", res2, err)
	}
	// the migrated vault loads through providers with the same meaning
	if got := providers.Pin(providers.PinProfile); got != "dev" {
		t.Errorf("Pin(CLAUDE_PROFILE) = %q", got)
	}
	for _, n := range []string{"dev", "qa", "prod", "digitalbtd-digitalbtd-user-qa", "personal", "max"} {
		if reg2.Providers[n] == nil {
			t.Errorf("profile %s missing after migration", n)
		}
	}
	if reg2.Providers["github-copilot"] != nil || reg2.Providers["qa"].Provider != "databricks" ||
		reg2.Providers["personal"].Provider != "github" {
		t.Error("profile → provider resolution changed")
	}
	if reg2.Providers["personal"].Login != "octocat" {
		t.Errorf("personal login = %q", reg2.Providers["personal"].Login)
	}
	if !slices.Equal(reg2.Fallback, []string{"max"}) {
		t.Errorf("fallback = %v", reg2.Fallback)
	}
	if !providers.CredentialPresent(reg2.Providers["qa"]) || !providers.CredentialPresent(reg2.Providers["digitalbtd-digitalbtd-user-qa"]) ||
		!providers.CredentialPresent(reg2.Providers["anthropic"]) {
		t.Error("credentials no longer resolve")
	}
	// the nested document is byte-stable
	doc, _ := providers.OverlayDoc()
	if !strings.Contains(string(doc), `"databricks": {`) {
		t.Errorf("overlay not nested:\n%s", doc)
	}
}

func TestEnvSecretRefKeepsOldName(t *testing.T) {
	fixture(t)
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Env, "ANTHROPIC_API_KEY", "@secret") }); err != nil {
		t.Fatal(err)
	}
	reg, _ := Current()
	if _, err := Apply(reg); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_KEY"} {
		if v, ok := read(t, n); !ok || v != "sk-ant-x" {
			t.Errorf("%s = %q,%v", n, v, ok)
		}
	}
}

func TestApplyAllOrNothing(t *testing.T) {
	fixture(t)
	reg, _ := Current()
	// a record that cannot be opened (sealed under another name) makes the secret step fail mid-transaction
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error {
		r, _, _ := tx.Record("TAVILY_API_KEY")
		return tx.PutRecord("ANTHROPIC_API_KEY", r)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(reg); err == nil {
		t.Fatal("apply must fail on an unopenable secret")
	}
	_ = vault.View(func(tx *vault.Tx) error {
		if _, ok := tx.Get(vault.Pins, "HARNESS_PROVIDER"); !ok || tx.Schema() != 2 {
			t.Error("a failed apply changed the vault")
		}
		return nil
	})
}

func TestNoVaultIsNoop(t *testing.T) {
	vaulttest.Use(t)
	reg, _ := providers.Load()
	res, err := Apply(reg)
	if err != nil || !res.Plan.Empty() || res.Backup != "" {
		t.Fatalf("%+v %v", res, err)
	}
}
