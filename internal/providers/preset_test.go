package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const testBase = `{"providers": {"anthropic": {"a": {"kind": "anthropic", "base_url": "https://a", "auth": {"type": "none"}}}}}`

func parseOverlay(t *testing.T, overlay string) (*Registry, error) {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv(PinProfile, "")
	return Parse([]byte(testBase), []byte(overlay))
}

func mustParse(t *testing.T, overlay string) *Registry {
	t.Helper()
	reg, err := parseOverlay(t, overlay)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestPresetExpansion(t *testing.T) {
	reg := mustParse(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "https://adb-1.azuredatabricks.net/", "cliProfile": "work"}}}}`)
	p := reg.Providers["dbx"]
	if p.Kind != DialectAnthropic || p.BaseURL != "https://adb-1.azuredatabricks.net/serving-endpoints/anthropic" {
		t.Errorf("kind/base = %q %q", p.Kind, p.BaseURL)
	}
	for _, d := range []string{DialectOpenAI, DialectResponses} {
		if got, ok := p.Route(d); !ok || got != "https://adb-1.azuredatabricks.net/serving-endpoints" {
			t.Errorf("Route(%s) = %q %v", d, got, ok)
		}
	}
	if got, _ := p.Route(DialectAnthropic); got != p.BaseURL {
		t.Errorf("Route(anthropic) = %q", got)
	}
	if p.Auth.Type != AuthKey || p.Auth.TTL != 1800 {
		t.Errorf("auth = %+v", p.Auth)
	}
	if p.Auth.Command != "databricks auth token -p work -o json | jq -r .access_token" {
		t.Errorf("command = %q", p.Auth.Command)
	}
	if p.Auth.RefreshCommand != "databricks auth token -p work --force-refresh -o json | jq -r .access_token" {
		t.Errorf("refresh = %q", p.Auth.RefreshCommand)
	}
	for _, c := range []string{p.Auth.Command, p.Auth.RefreshCommand} {
		if strings.Contains(c, " -t ") {
			t.Errorf("command uses -t: %q", c)
		}
	}
	wantModels := map[string]string{"opus": "databricks-claude-opus-5-5", "sonnet": "databricks-claude-sonnet-5-5", "haiku": "databricks-claude-haiku-5-5"}
	if !reflect.DeepEqual(p.Models, wantModels) {
		t.Errorf("models = %v", p.Models)
	}
	if !reflect.DeepEqual(p.Env, map[string]string{"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"}) {
		t.Errorf("env = %v", p.Env)
	}
}

func TestPresetWithoutCLIProfileLeavesCommandsEmpty(t *testing.T) {
	p := mustParse(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "https://h.example.com"}}}}`).Providers["dbx"]
	if p.Auth.Command != "" || p.Auth.RefreshCommand != "" {
		t.Errorf("commands derived without cliProfile: %+v", p.Auth)
	}
}

func TestPresetExplicitFieldsWin(t *testing.T) {
	p := mustParse(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "https://h.example.com", "cliProfile": "work",
		"kind": "openai", "base_url": "https://own/base", "routes": {"openai": "https://own/oa"},
		"auth": {"type":"key","header":"x-api-key", "ttl": 60, "command": "echo mine", "refreshCommand": "echo fresh"},
		"models": {"opus": "my-opus"}, "env": {"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "0", "OTHER": "x"}}}}}`).Providers["dbx"]
	if p.Kind != "openai" || p.BaseURL != "https://own/base" {
		t.Errorf("kind/base overridden: %q %q", p.Kind, p.BaseURL)
	}
	if got, _ := p.Route(DialectOpenAI); got != "https://own/oa" {
		t.Errorf("openai route = %q", got)
	}
	if got, _ := p.Route(DialectResponses); got != "https://h.example.com/serving-endpoints" {
		t.Errorf("responses route = %q", got)
	}
	if p.Auth.Type != AuthKey || p.Auth.Header != HeaderXAPIKey || p.Auth.TTL != 60 || p.Auth.Command != "echo mine" || p.Auth.RefreshCommand != "echo fresh" {
		t.Errorf("auth overridden: %+v", p.Auth)
	}
	if p.Models["opus"] != "my-opus" || p.Models["sonnet"] != "databricks-claude-sonnet-5-5" {
		t.Errorf("models = %v", p.Models)
	}
	if p.Env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] != "0" || p.Env["OTHER"] != "x" {
		t.Errorf("env = %v", p.Env)
	}
}

func TestNoPresetProviderUnchanged(t *testing.T) {
	reg := mustParse(t, `{"providers": {"anthropic": {"plain": {"kind": "anthropic", "base_url": "https://p", "auth": {"type": "none"}}}}}`)
	p := reg.Providers["plain"]
	if p.Models != nil || p.Env != nil || p.Routes != nil || p.BaseURL != "https://p" || p.Auth.TTL != 0 || p.Auth.Command != "" {
		t.Errorf("no-preset provider changed: %+v", p)
	}
}

func TestExpandPresetsDoesNotMutateSharedMaps(t *testing.T) {
	shared := map[string]string{"opus": "x"}
	p := &Provider{Name: "d", Preset: PresetDatabricks, Host: "https://h.example.com", Models: shared}
	expandPresets(&Registry{Providers: map[string]*Provider{"d": p}})
	if len(shared) != 1 || len(p.Models) != 3 {
		t.Errorf("shared = %v, models = %v", shared, p.Models)
	}
}

func TestValidatePresetAndEnvRejections(t *testing.T) {
	const ok = `"preset": "databricks", "host": "https://h.example.com"`
	const plain = `"kind": "anthropic", "base_url": "https://x", "auth": {"type": "none"}`
	cases := []struct{ name, entry, want string }{
		{"unknown preset", `"preset": "nope", "host": "https://h.example.com"`, "unknown preset"},
		{"http host", `"preset": "databricks", "host": "http://h.example.com"`, "absolute https"},
		{"host with path", `"preset": "databricks", "host": "https://h.example.com/x"`, "absolute https"},
		{"relative host", `"preset": "databricks", "host": "h.example.com"`, "absolute https"},
		{"missing host", `"preset": "databricks"`, "absolute https"},
		{"bad cliProfile", ok + `, "cliProfile": "a b; rm"`, "cliProfile"},
		{"host without preset", plain + `, "host": "https://h.example.com"`, "need a preset"},
		{"cliProfile without preset", plain + `, "cliProfile": "w"`, "need a preset"},
		{"lowercase env key", ok + `, "env": {"lower": "1"}`, "env key"},
		{"token env key", ok + `, "env": {"MY_TOKEN": "1"}`, "credential"},
		{"api key env key", ok + `, "env": {"ANTHROPIC_API_KEY": "1"}`, "credential"},
		{"secret env key", ok + `, "env": {"X_SECRET": "1"}`, "credential"},
		{"password env key", ok + `, "env": {"DB_PASSWORD": "1"}`, "credential"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseOverlay(t, `{"providers": {"databricks": {"dbx": {`+c.entry+`}}}}`)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

// Expansion is load-only: writing one field of a preset overlay entry must not persist derived fields.
func TestExpansionNeverLeaksIntoOverlay(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.UseKeyring(t)
	if err := SetProviderFields("dbx", map[string]any{"preset": "databricks", "host": "https://h.example.com", "cliProfile": "w"}); err != nil {
		t.Fatal(err)
	}
	if err := SetProviderModel("dbx", "opus", "my-opus"); err != nil {
		t.Fatal(err)
	}
	doc, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"serving-endpoints", "databricks auth token", "EXPERIMENTAL_BETAS", "databricks-claude"} {
		if strings.Contains(string(doc), leaked) {
			t.Errorf("overlay leaked derived %q:\n%s", leaked, doc)
		}
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := reg.Providers["dbx"]; p == nil || p.BaseURL == "" || p.Models["opus"] != "my-opus" {
		t.Errorf("reloaded provider = %+v", p)
	}
}

func jwtExpIn(d time.Duration, iss string) string {
	return fakeJWT(fmt.Sprintf(`{"iss":%q,"exp":%d}`, iss, time.Now().Add(d).Unix()))
}

// credProvider builds a preset provider whose commands append to a counter file, then print a crafted JWT.
func credProvider(t *testing.T, name, cmdTok, refreshTok string, ttl int) (*Provider, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.UseKeyring(t)
	cmdCache.Delete(name)
	t.Cleanup(func() { cmdCache.Delete(name) })
	log := filepath.Join(t.TempDir(), "runs")
	p := &Provider{Name: name, Preset: PresetDatabricks, Host: "https://h.example.com", Auth: Auth{
		Type: AuthKey, TTL: ttl,
		Command:        fmt.Sprintf("echo cmd >> %s; printf '%%s' '%s'", log, cmdTok),
		RefreshCommand: fmt.Sprintf("echo refresh >> %s; printf '%%s' '%s'", log, refreshTok),
	}}
	return p, log
}

func runs(log string) string {
	b, _ := os.ReadFile(log)
	return strings.Join(strings.Fields(string(b)), ",")
}

func TestNearExpiryRunsRefreshCommand(t *testing.T) {
	near, fresh := jwtExpIn(5*time.Minute, "https://h.example.com/oidc"), jwtExpIn(time.Hour, "https://h.example.com/oidc")
	p, log := credProvider(t, "dbx-near", near, fresh, 1800)
	got, err := commandCredential(context.Background(), p, false)
	if err != nil || got != fresh {
		t.Fatalf("got %q, %v; want refreshed token", got, err)
	}
	if r := runs(log); r != "cmd,refresh" {
		t.Errorf("runs = %s", r)
	}
}

func TestFarFromExpiryDoesNotRefresh(t *testing.T) {
	far := jwtExpIn(time.Hour, "https://h.example.com/oidc")
	p, log := credProvider(t, "dbx-far", far, jwtExpIn(2*time.Hour, "https://h.example.com/oidc"), 1800)
	got, err := commandCredential(context.Background(), p, false)
	if err != nil || got != far {
		t.Fatalf("got %q, %v", got, err)
	}
	if r := runs(log); r != "cmd" {
		t.Errorf("runs = %s", r)
	}
}

func TestNonJWTCredentialUnchanged(t *testing.T) {
	p, log := credProvider(t, "dbx-opaque", "opaque-token", "other", 1800)
	got, err := commandCredential(context.Background(), p, false)
	if err != nil || got != "opaque-token" {
		t.Fatalf("got %q, %v", got, err)
	}
	if r := runs(log); r != "cmd" {
		t.Errorf("runs = %s", r)
	}
}

func TestForceUsesRefreshCommand(t *testing.T) {
	p, log := credProvider(t, "dbx-force", jwtExpIn(time.Hour, "https://h.example.com"), "forced", 1800)
	got, err := commandCredential(context.Background(), p, true)
	if err != nil || got != "forced" {
		t.Fatalf("got %q, %v", got, err)
	}
	if r := runs(log); r != "refresh" {
		t.Errorf("runs = %s", r)
	}
}

func TestCacheCappedAtExpiryMinusMargin(t *testing.T) {
	// exp in 15m, ttl 30m: cache ends near exp-10m (~5m), not 30m.
	tok := jwtExpIn(15*time.Minute, "https://h.example.com")
	p, _ := credProvider(t, "dbx-cap", tok, tok, 1800)
	if _, err := commandCredential(context.Background(), p, false); err != nil {
		t.Fatal(err)
	}
	c, ok := cmdCache.Load("dbx-cap")
	if !ok {
		t.Fatal("not cached")
	}
	left := time.Until(c.(cached).until)
	if left > 5*time.Minute+5*time.Second || left < 4*time.Minute {
		t.Errorf("cache lifetime = %v, want ~5m", left)
	}
}

func TestCacheUsesTTLWhenTokenOutlivesIt(t *testing.T) {
	tok := jwtExpIn(24*time.Hour, "https://h.example.com")
	p, _ := credProvider(t, "dbx-ttl", tok, tok, 60)
	if _, err := commandCredential(context.Background(), p, false); err != nil {
		t.Fatal(err)
	}
	c, _ := cmdCache.Load("dbx-ttl")
	if left := time.Until(c.(cached).until); left > 61*time.Second || left <= 50*time.Second {
		t.Errorf("cache lifetime = %v, want in (50s, 60s]", left)
	}
}

func TestIssuerMismatchErrorsWithoutToken(t *testing.T) {
	tok := jwtExpIn(time.Hour, "https://other.example.org/oidc")
	p, _ := credProvider(t, "dbx-iss", tok, tok, 1800)
	_, err := commandCredential(context.Background(), p, false)
	if err == nil {
		t.Fatal("want issuer mismatch error")
	}
	for _, want := range []string{"dbx-iss", "other.example.org", "h.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), strings.Split(tok, ".")[1]) {
		t.Errorf("error leaks token: %v", err)
	}
	if _, cached := cmdCache.Load("dbx-iss"); cached {
		t.Error("mismatched token was cached")
	}
}

func TestIssuerCheckSkippedWithoutPreset(t *testing.T) {
	tok := jwtExpIn(time.Hour, "https://other.example.org")
	p, _ := credProvider(t, "plain-iss", tok, tok, 1800)
	p.Preset, p.Host = "", ""
	if got, err := commandCredential(context.Background(), p, false); err != nil || got != tok {
		t.Errorf("got %q, %v", got, err)
	}
}
