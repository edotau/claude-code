package databricks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestPairingPlanSignalsAndSafety(t *testing.T) {
	for _, tc := range []struct {
		name, cfg string
		choices   map[string]string
		want      string
	}{
		{name: "automatic", cfg: pairingCfg, want: "dev-client"},
		{name: "cross host", cfg: strings.Replace(pairingCfg, "[dev-sp]\nhost = https://dev.", "[dev-sp]\nhost = https://other.", 1)},
		{name: "ambiguous", cfg: pairingCfg + "\n[other-sp]\nhost = https://dev.example.cloud.databricks.com\nclient_id = other\nclient_secret = value\n"},
		{name: "comment", cfg: strings.Replace(pairingCfg, "[dev-sp]", "# claude-code: pair=dev\n[dev-sp]", 1) + "\n[other]\nhost = https://dev.example.cloud.databricks.com\n", want: "dev-client"},
		{name: "comment cross host", cfg: strings.Replace(strings.Replace(pairingCfg, "[dev-sp]", "# claude-code: pair=dev\n[dev-sp]", 1), "[dev-sp]\nhost = https://dev.", "[dev-sp]\nhost = https://other.", 1)},
		{name: "none", cfg: pairingCfg, choices: map[string]string{"dev-sp": ""}},
		{name: "explicit choice", cfg: pairingCfg + "\n[other]\nhost = https://dev.example.cloud.databricks.com\n", choices: map[string]string{"dev-sp": "dev"}, want: "dev-client"},
		{name: "choice beats comment", cfg: strings.Replace(pairingCfg, "[dev-sp]", "# claude-code: pair=missing\n[dev-sp]", 1), choices: map[string]string{"dev-sp": "dev"}, want: "dev-client"},
		{name: "env does not guess", cfg: pairingCfg + "\n[other]\nhost = https://dev.example.cloud.databricks.com\nenv = qa\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := candidatesOf(t, tc.cfg)
			rows := ImportPlan(cs, nil, ImportOptions{Pair: tc.choices})
			top := map[string]json.RawMessage{}
			if err := Apply(top, rows); err != nil {
				t.Fatal(err)
			}
			var dev providers.Provider
			if err := json.Unmarshal(Existing(top)["dev"], &dev); err != nil {
				t.Fatal(err)
			}
			if dev.Auth.SPClientID != tc.want {
				t.Fatalf("spClientId = %q, want %q", dev.Auth.SPClientID, tc.want)
			}
			_, standalone := Existing(top)["dev-sp"]
			if standalone != (tc.want == "") {
				t.Fatalf("standalone=%v; rows=%+v", standalone, rows)
			}
			if tc.name == "ambiguous" && !strings.Contains(rows[1].Reason, "configure") {
				t.Fatalf("missing configure pointer: %+v", rows[1])
			}
		})
	}
}

func TestPairingReplacePreservesDecisionAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		changeHost      bool
	}{
		{name: "existing pairing", raw: `{"preset":"databricks","host":"https://dev.example.cloud.databricks.com","auth":{"type":"oauth","spClientId":"original"},"identity":"user"}`, want: "original"},
		{name: "explicit unpair", raw: `{"preset":"databricks","host":"https://dev.example.cloud.databricks.com","auth":{"type":"oauth","spClientId":""},"identity":"user"}`},
		{name: "host move", raw: `{"preset":"databricks","host":"https://dev.example.cloud.databricks.com","auth":{"type":"oauth","spClientId":"dev-client"},"identity":"user"}`, want: "dev-client", changeHost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pairingCfg
			if tc.changeHost {
				cfg = strings.ReplaceAll(cfg, "https://dev.", "https://new.")
			}
			top := map[string]json.RawMessage{"providers": json.RawMessage(`{"dev":` + tc.raw + `}`)}
			rows := ImportPlan(candidatesOf(t, cfg), Existing(top), ImportOptions{Replace: true, Pair: map[string]string{"dev-sp": "dev"}})
			if tc.changeHost && rows[0].Action != "skip" {
				t.Fatalf("host changed: %+v", rows[0])
			}
			if err := Apply(top, rows); err != nil {
				t.Fatal(err)
			}
			var dev providers.Provider
			_ = json.Unmarshal(Existing(top)["dev"], &dev)
			if dev.Auth.SPClientID != tc.want || dev.Identity != "user" {
				t.Fatalf("decision changed: %+v", dev)
			}
		})
	}
}

func TestPairingHonorsRenamesAndSecretCollisions(t *testing.T) {
	cs := candidatesOf(t, pairingCfg)
	rows := ImportPlan(cs, nil, ImportOptions{Rename: map[string]string{"dev": "my-dev"}})
	if rows[0].SPClientID != "dev-client" || SecretNameOf(rows[0]) != "DATABRICKS_MY_DEV_M2M" {
		t.Fatalf("renamed pairing: %+v", rows)
	}
	existing := map[string]json.RawMessage{"my_dev": json.RawMessage(`{"auth":{"type":"m2m","clientId":"other"}}`)}
	rows = ImportPlan(cs, existing, ImportOptions{Rename: map[string]string{"dev": "my-dev"}})
	if rows[0].SPClientID != "" || rows[1].Action == "skip" {
		t.Fatalf("paired secret collision: %+v", rows)
	}
}

func TestPairingExportRoundTripAndCommentMerge(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": {
		Preset: "databricks", Host: "https://dev.example.cloud.databricks.com", Auth: providers.Auth{Type: providers.AuthOAuth, SPClientID: "dev-client"},
	}}}
	rows, skipped := ExportRows(reg)
	if len(rows) != 2 || len(skipped) != 0 {
		t.Fatalf("export rows=%v skipped=%v", rows, skipped)
	}
	rows, skipped = Rename(rows, map[string]string{"dev": "renamed"})
	if len(skipped) != 0 {
		t.Fatal(skipped)
	}
	text := RenderExport(rows)
	if !strings.Contains(text, "# claude-code: pair=renamed\n[dev-sp]") || strings.Contains(text, "client_secret") {
		t.Fatalf("export: %s", text)
	}
	plan := ImportPlan(candidatesOf(t, text), nil, ImportOptions{})
	if plan[0].SPClientID != "dev-client" || plan[1].Action != "skip" {
		t.Fatalf("round trip: %+v", plan)
	}
	if !strings.Contains(Hint(rows[1]), "DATABRICKS_DEV_M2M") {
		t.Fatal("hint uses standalone SP's secret")
	}
	existing := "[other]\nhost = https://other\nauth_type = pat\n\n# claude-code: pair=dev\n[svc]\nhost = https://dev\nclient_id = old\nauth_type = oauth-m2m\n"
	merged, _ := Merge(existing, []ExportRow{{ID: "other", Section: "other", Host: "https://new", Auth: providers.AuthPAT}, {ID: "svc", Section: "svc", Host: "https://dev", Auth: providers.AuthM2M, ClientID: "new", Pair: "other", PairSet: true}}, true)
	cs := candidatesOf(t, merged)
	if len(cs) != 2 || !cs[1].PairSet || cs[1].Pair != "dev" {
		t.Fatalf("pair comment lost during merge: %s", merged)
	}
	if strings.Count(merged, "# claude-code: pair=") != 1 {
		t.Fatalf("duplicate pairing signal: %s", merged)
	}
}

func TestPairingExportUnpairedStaysUnpaired(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"dev":    {Preset: "databricks", Host: "https://dev.example.cloud.databricks.com", Auth: providers.Auth{Type: providers.AuthOAuth}},
		"dev-sp": {Preset: "databricks", Host: "https://dev.example.cloud.databricks.com", Auth: providers.Auth{Type: providers.AuthM2M, ClientID: "dev-client"}},
	}}
	rows, _ := ExportRows(reg)
	text := RenderExport(rows)
	if !strings.Contains(text, "# claude-code: pair=none\n[dev-sp]") {
		t.Fatalf("missing unpair signal: %s", text)
	}
	plan := ImportPlan(candidatesOf(t, text), nil, ImportOptions{})
	if plan[0].SPClientID != "" || plan[1].Action == "skip" {
		t.Fatalf("export paired unpaired rows: %+v", plan)
	}
}
