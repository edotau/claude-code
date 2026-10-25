package profile

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const twinPair = `"databricks-dev":{"kind":"anthropic","base_url":"https://adb-1.azuredatabricks.net/serving-endpoints/anthropic",
"auth":{"type":"bearer","command":"databricks auth token -p DEVPROF | jq -r .access_token"},
"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-5-5"}},
"databricks-dev-oai":{"kind":"openai","base_url":"https://adb-1.azuredatabricks.net/serving-endpoints",
"auth":{"type":"bearer","command":"databricks auth token -p DEVPROF | jq -r .access_token"},
"models":{"opus":"databricks-gpt-5-5"}}`

func seed(t *testing.T, doc string) {
	t.Helper()
	vaulttest.Use(t)
	for _, k := range []string{providers.PinProvider, providers.PinModel} {
		t.Setenv(k, "")
	}
	if doc != "" {
		vaulttest.Put(t, vault.Overlay, "providers", doc)
	}
}

func rawDoc(t *testing.T) string {
	t.Helper()
	var s string
	if err := vault.View(func(tx *vault.Tx) error { s, _ = tx.Get(vault.Overlay, "providers"); return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"own", "databricks-dev", "a.b_c", "7x"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", ".x", "a b", "a/b", "a;b", "é"} {
		if CheckName(bad) == nil {
			t.Errorf("CheckName(%q) accepted", bad)
		}
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	seed(t, `{"providers":{`+twinPair+`}}`)
	before := rawDoc(t)
	_, newDoc, notes, err := Migrate(false)
	if err != nil || len(newDoc) == 0 || len(notes) == 0 {
		t.Fatalf("dry run: err %v notes %v", err, notes)
	}
	if rawDoc(t) != before {
		t.Fatal("dry run changed the vault")
	}
	if strings.Contains(newDoc2(t, newDoc), "databricks-dev-oai") {
		t.Error("planned doc still holds the -oai row")
	}
}

func newDoc2(t *testing.T, b []byte) string { t.Helper(); return string(b) }

func TestMigrateApplyAndIdempotent(t *testing.T) {
	seed(t, `{"providers":{`+twinPair+`}}`)
	if _, _, _, err := Migrate(true); err != nil {
		t.Fatal(err)
	}
	var top struct {
		Providers map[string]providers.Provider `json:"providers"`
		Profiles  providers.Profiles            `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(rawDoc(t)), &top); err != nil {
		t.Fatal(err)
	}
	dev, ok := top.Providers["databricks-dev"]
	if !ok || dev.Preset != "databricks" || dev.Host != "https://adb-1.azuredatabricks.net" || dev.CLIProfile != "DEVPROF" {
		t.Fatalf("databricks-dev = %+v", dev)
	}
	if !strings.Contains(dev.Auth.Command, "-p DEVPROF") {
		t.Errorf("auth.command not kept: %q", dev.Auth.Command)
	}
	if _, ok := top.Providers["databricks-dev-oai"]; ok {
		t.Error("-oai row not deleted")
	}
	set := top.Profiles.Sets["databricks-dev"]
	for _, a := range []string{"session", "copilot", "opencode", "codex"} {
		if set[a].Account != "databricks-dev" {
			t.Errorf("set row %s = %+v", a, set[a])
		}
	}
	if _, ok := top.Profiles.Sets["own"]; !ok {
		t.Error("own set missing")
	}
	if top.Profiles.Active != "" {
		t.Errorf("migrate set active = %q", top.Profiles.Active)
	}
	first := rawDoc(t)
	_, _, notes, err := Migrate(true)
	if err != nil {
		t.Fatal(err)
	}
	if rawDoc(t) != first {
		t.Error("second migrate changed the doc")
	}
	var buf bytes.Buffer
	if PrintDiff(&buf, []byte(first), []byte(rawDoc(t))) {
		t.Errorf("second run reports a diff: %s (notes %v)", buf.String(), notes)
	}
}

func TestMigrateRefusesDanglingRefs(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"fallback": func(t *testing.T) string {
			return `{"fallback":["databricks-dev-oai"],"providers":{` + twinPair + `}}`
		},
		"pin": func(t *testing.T) string {
			vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "databricks-dev-oai:x")
			return `{"providers":{` + twinPair + `}}`
		},
		"env": func(t *testing.T) string {
			vaulttest.Put(t, vault.Env, "SOME_VAR", "databricks-dev-oai")
			return `{"providers":{` + twinPair + `}}`
		},
		"vendors": func(t *testing.T) string {
			vaulttest.Put(t, vault.Overlay, "vendors", `{"x":"databricks-dev-oai"}`)
			return `{"providers":{` + twinPair + `}}`
		},
		"preset hand-edited": func(t *testing.T) string {
			return strings.Replace(`{"providers":{`+twinPair+`}}`, `"databricks-dev":{"kind":"anthropic"`,
				`"databricks-dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","kind":"anthropic"`, 1)
		},
		"unparseable -p": func(t *testing.T) string {
			return strings.Replace(`{"providers":{`+twinPair+`}}`, "databricks auth token -p DEVPROF | jq",
				"databricks auth token | jq", 1)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			seed(t, "")
			doc := mk(t)
			vaulttest.Put(t, vault.Overlay, "providers", doc)
			before := rawDoc(t)
			if _, _, _, err := Migrate(true); err == nil {
				t.Fatal("migrate accepted a refusable overlay")
			}
			if rawDoc(t) != before {
				t.Error("refused migrate changed the providers doc")
			}
			var prev string
			_ = vault.View(func(tx *vault.Tx) error { prev, _ = tx.Get(vault.Overlay, "providers.prev"); return nil })
			if prev != "" {
				t.Error("refused migrate wrote providers.prev")
			}
		})
	}
}

func TestUseOffRoundTrip(t *testing.T) {
	seed(t, `{"providers":{`+twinPair+`}}`)
	if _, _, _, err := Migrate(true); err != nil {
		t.Fatal(err)
	}
	post := rawDoc(t)
	if _, _, err := Use(false, "databricks-dev"); err != nil || rawDoc(t) != post {
		t.Fatalf("use dry-run: err %v, changed %v", err, rawDoc(t) != post)
	}
	if _, _, err := Use(true, "nope"); err == nil {
		t.Error("use of a missing set accepted")
	}
	if _, _, err := Use(true, "databricks-dev"); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil || reg.Profiles.Active != "databricks-dev" {
		t.Fatalf("active = %q, err %v", reg.Profiles.Active, err)
	}
	if _, _, err := Off(true); err != nil {
		t.Fatal(err)
	}
	if rawDoc(t) != post {
		t.Errorf("use+off did not restore the doc\nwant %s\ngot  %s", post, rawDoc(t))
	}
}

func TestSaveRefusesOverwriteWithoutForce(t *testing.T) {
	seed(t, "")
	set := providers.Profile{"session": {Account: "anthropic"}}
	if err := Save("mine", set, false); err != nil {
		t.Fatal(err)
	}
	first := rawDoc(t)
	if err := Save("mine", providers.Profile{"session": {Account: "openai"}}, false); err == nil {
		t.Fatal("overwrite without force accepted")
	}
	if rawDoc(t) != first {
		t.Error("refused save changed the doc")
	}
	if err := Save("mine", providers.Profile{"session": {Account: "openai"}}, true); err != nil {
		t.Fatal(err)
	}
	if err := Save("bad name", set, true); err == nil {
		t.Error("bad name accepted")
	}
}

func TestShowNeverPrintsCredentialsAndReportsDrift(t *testing.T) {
	seed(t, `{"providers":{
	  "viacmd":{"kind":"anthropic","base_url":"https://c.example/anthropic","auth":{"type":"bearer","command":"echo SENTINELCMD"},"models":{"opus":"m"}},
	  "viaenv":{"kind":"anthropic","base_url":"https://e.example/anthropic","auth":{"type":"bearer","env":"SENTINEL_KEY_VAR"},"models":{"opus":"m"}}},
	  "profiles":{"active":"s","sets":{"s":{"session":{"account":"viacmd","models":{"opus":"m"}},"copilot":{"account":"viaenv","model":"m"}}}}}`)
	t.Setenv("SENTINEL_KEY_VAR", "SENTINELENV")
	t.Setenv(providers.PinProvider, "viaenv")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Show(&buf, reg, "s"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "SENTINEL") {
		t.Errorf("Show printed a credential:\n%s", out)
	}
	if !strings.Contains(out, "drift: "+providers.PinProvider+"=viaenv") {
		t.Errorf("no drift line:\n%s", out)
	}
	if err := Show(&buf, reg, "missing"); err == nil {
		t.Error("show of a missing set succeeded")
	}
}
