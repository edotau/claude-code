package providers

import (
	"strings"
	"testing"
)

const dbxProv = `"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net/","cliProfile":"work"}`

func dbxProvider(t *testing.T) Provider {
	t.Helper()
	return *mustParse(t, `{"providers":{`+dbxProv+`}}`).Providers["dbx"]
}

func TestWireModelTable(t *testing.T) {
	db := dbxProvider(t)
	plain := Provider{}
	cases := []struct {
		name string
		p    Provider
		in   string
		want string
	}{
		{"canonical", db, "claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"already wire", db, "databricks-claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"1m kept", db, "claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{"wire 1m unchanged", db, "databricks-claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{"slash qualified", db, "openai/gpt-5", "openai/gpt-5"},
		{"colon qualified", db, "openai:gpt-5", "openai:gpt-5"},
		{"empty", db, "", ""},
		{"dots to dashes", db, "gpt-5.5", "databricks-gpt-5-5"},
		{"dots with 1m", db, "claude-opus-4.5[1m]", "databricks-claude-opus-4-5[1m]"},
		{"custom name unchanged", db, "my-opus", "my-opus"},
		{"custom dotted name unchanged", db, "my.endpoint", "my.endpoint"},
		{"catalog gemini", db, "gemini-3.8-flash", "databricks-gemini-3-8-flash"},
		{"non-catalog passes through", db, "gemini-2.5-pro", "gemini-2.5-pro"},
		{"no prefix provider", plain, "claude-opus-5-5", "claude-opus-5-5"},
		{"no prefix keeps dots", plain, "gpt-5.5", "gpt-5.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.p.WireModel(c.in)
			if got != c.want {
				t.Fatalf("WireModel(%q) = %q, want %q", c.in, got, c.want)
			}
			if again := c.p.WireModel(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestCanonicalModelRoundTrip(t *testing.T) {
	db := dbxProvider(t)
	for _, id := range []string{"claude-opus-5-5", "claude-opus-5-5[1m]", "gpt-5-5", "claude-haiku-5-5"} {
		if got := db.CanonicalModel(db.WireModel(id)); got != id {
			t.Errorf("round trip %q -> %q", id, got)
		}
	}
	if got := db.CanonicalModel("databricks-claude-opus-5-5[1m]"); got != "claude-opus-5-5[1m]" {
		t.Errorf("CanonicalModel 1m = %q", got)
	}
	if got := (Provider{}).CanonicalModel("x-y"); got != "x-y" {
		t.Errorf("no-prefix CanonicalModel = %q", got)
	}
}

func TestLoadDecoratesPresetModels(t *testing.T) {
	reg := mustParse(t, `{"providers":{"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net/","cliProfile":"work",
	  "models":{"opus":"claude-opus-5-5[1m]","sonnet":"gpt-5.5"}}}}`)
	m := reg.Providers["dbx"].Models
	want := map[string]string{
		"opus": "databricks-claude-opus-5-5[1m]", "sonnet": "databricks-gpt-5-5", "haiku": "databricks-claude-haiku-5-5",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Models[%s] = %q, want %q", k, m[k], v)
		}
	}
	plain := mustParse(t, `{"providers":{"p":{"kind":"anthropic","base_url":"https://p","auth":{"type":"none"},"models":{"opus":"gpt-5.5"}}}}`)
	if got := plain.Providers["p"].Models["opus"]; got != "gpt-5.5" {
		t.Errorf("plain provider model = %q", got)
	}
}

// The decorated ids live only in memory: the overlay writer works on the stored doc, never the registry.
func TestDecoratedModelsNeverPersisted(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	err := SetProviderFields("dbx", map[string]any{"preset": "databricks", "host": "https://adb-1.azuredatabricks.net/",
		"cliProfile": "work", "models": map[string]string{"opus": "claude-opus-5-5"}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Providers["dbx"].Models["opus"]; got != "databricks-claude-opus-5-5" {
		t.Fatalf("loaded opus = %q, want decorated", got)
	}
	if err := SetProviderModel("dbx", "sonnet", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	doc, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "databricks-claude") {
		t.Errorf("overlay persisted decorated ids: %s", doc)
	}
	if !strings.Contains(string(doc), `"claude-opus-5-5"`) || !strings.Contains(string(doc), `"claude-sonnet-5-5"`) {
		t.Errorf("overlay lost the canonical ids: %s", doc)
	}
}
