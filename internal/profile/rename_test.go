package profile

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// twinsFor is the databricks-<env> twin pair for each env, joined into one providers doc.
func twinsFor(envs ...string) string {
	var parts []string
	for _, e := range envs {
		parts = append(parts, strings.NewReplacer("databricks-dev", "databricks-"+e, "DEVPROF", strings.ToUpper(e)+"PROF").Replace(twinPair))
	}
	return `{"providers":{` + strings.Join(parts, ",") + `}}`
}

type migrated struct {
	Providers map[string]providers.Provider `json:"providers"`
	Profiles  providers.Profiles            `json:"profiles"`
}

func readMigrated(t *testing.T) migrated {
	t.Helper()
	var m migrated
	if err := json.Unmarshal([]byte(rawDoc(t)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMigrateRenamesEnvRowsAndCreatesDefaultSet(t *testing.T) {
	seed(t, twinsFor("dev", "qa", "prod"))
	if _, _, _, err := Migrate(true); err != nil {
		t.Fatal(err)
	}
	m := readMigrated(t)
	for _, e := range []string{"dev", "qa", "prod"} {
		if p, ok := m.Providers[e]; !ok || p.Preset != providers.PresetDatabricks || p.CLIProfile != strings.ToUpper(e)+"PROF" {
			t.Errorf("row %s = %+v", e, p)
		}
		for _, old := range []string{"databricks-" + e, "databricks-" + e + "-oai"} {
			if _, ok := m.Providers[old]; ok {
				t.Errorf("legacy row %s survived", old)
			}
		}
		if _, ok := m.Profiles.Sets["databricks-"+e]; ok {
			t.Errorf("legacy set databricks-%s survived", e)
		}
	}
	def, ok := m.Profiles.Sets[DefaultSet]
	if !ok || def["session"].Account != "dev" {
		t.Errorf("DEFAULT set = %+v (want accounts on dev)", def)
	}
	if DefaultSet != "DEFAULT" {
		t.Errorf("DefaultSet = %q", DefaultSet)
	}
}

func TestMigrateDryRunWritesNothingAcrossEnvs(t *testing.T) {
	seed(t, twinsFor("dev", "qa", "prod"))
	if err := secrets.Store("PROVIDER_DATABRICKS_QA_PAT", "sealed-sentinel-value-1234567890"); err != nil {
		t.Fatal(err)
	}
	before := rawDoc(t)
	_, newDoc, notes, err := Migrate(false)
	if err != nil || len(notes) == 0 || !strings.Contains(string(newDoc), `"qa"`) {
		t.Fatalf("dry run: err %v notes %v", err, notes)
	}
	if rawDoc(t) != before {
		t.Fatal("dry run changed the providers doc")
	}
	var prev string
	_ = vault.View(func(tx *vault.Tx) error { prev, _ = tx.Get(vault.Overlay, "providers.prev"); return nil })
	if prev != "" {
		t.Error("dry run wrote providers.prev")
	}
}

func TestMigrateRefusesDanglingRefToRenamedRow(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"pin": func(t *testing.T) { vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "databricks-qa:x") },
		"env": func(t *testing.T) { vaulttest.Put(t, vault.Env, "SOME_VAR", "databricks-qa") },
		"vendors": func(t *testing.T) {
			vaulttest.Put(t, vault.Overlay, "vendors", `{"x":"databricks-qa"}`)
		},
	}
	for name, arm := range cases {
		t.Run(name, func(t *testing.T) {
			seed(t, `{"providers":{"databricks-qa":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net"}}}`)
			arm(t)
			before := rawDoc(t)
			if _, _, _, err := Migrate(true); err == nil || !strings.Contains(err.Error(), "databricks-qa") {
				t.Fatalf("migrate accepted a dangling reference: %v", err)
			}
			if rawDoc(t) != before {
				t.Error("refused migrate changed the doc")
			}
		})
	}
}

func TestMigrateMovesSealedSecretWithRow(t *testing.T) {
	seed(t, strings.Replace(twinsFor("qa"), `"auth":{"type":"bearer","command"`, `"auth":{"type":"bearer","authType":"pat","command"`, 1))
	const val = "sealed-sentinel-value-1234567890"
	if err := secrets.Store("PROVIDER_DATABRICKS_QA_PAT", val); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Migrate(true); err != nil {
		t.Fatal(err)
	}
	if got, _, err := secrets.Lookup("PROVIDER_QA_PAT"); err != nil || got != val {
		t.Errorf("secret not under the new name: %q %v", got, err)
	}
	if got, _, _ := secrets.Lookup("PROVIDER_DATABRICKS_QA_PAT"); got != "" {
		t.Error("old secret name still present")
	}
}

func TestMigrateNotesHarnessProviderCompat(t *testing.T) {
	seed(t, twinsFor("qa"))
	if _, _, notes, err := Migrate(false); err != nil || strings.Contains(strings.Join(notes, "\n"), providers.PinProvider+"=") {
		t.Fatalf("no env set must give no compat note: %v %v", err, notes)
	}
	t.Setenv(providers.PinProvider, "databricks-qa")
	_, _, notes, err := Migrate(false)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(notes, "\n")
	if !strings.Contains(all, providers.PinProvider+"=databricks-qa") || !strings.Contains(all, "set it to qa") {
		t.Errorf("compat note missing:\n%s", all)
	}
}
