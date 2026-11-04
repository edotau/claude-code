package databricks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

const fixture = `[DEFAULT]
host = https://leak.example.cloud.databricks.com
token = dapiDEFAULTLEAK

[__settings__]
x = y

[alpha]
host = https://alpha.example.cloud.databricks.com/?o=1
auth_type = databricks-cli

[beta]
host = beta.example.cloud.databricks.com
client_id = cid-beta
client_secret = shh-beta

[gamma]
host = https://gamma.example.cloud.databricks.com
token = dapi0123

[delta]
host = http://insecure.example.com

[bad name]
host = https://bad.example.cloud.databricks.com
`

func writeFixtureCfg(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCandidates(t *testing.T) {
	cs, err := Candidates(writeFixtureCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Candidate{}
	for _, c := range cs {
		got[c.Name] = c
	}
	if _, ok := got["DEFAULT"]; ok || got["__settings__"].Name != "" {
		t.Fatalf("DEFAULT/__settings__ leaked: %v", got)
	}
	checkFixtureAuth(t, got)
	if got["delta"].Skip == "" || got["beta"].ClientID != "cid-beta" || !got["beta"].HasSecret {
		t.Errorf("delta/beta: %+v %+v", got["delta"], got["beta"])
	}
	checkNoSecretInCandidates(t, cs)
}

// checkFixtureAuth checks the auth type and normalized host of the fixture's usable profiles.
func checkFixtureAuth(t *testing.T, got map[string]Candidate) {
	t.Helper()
	want := map[string][3]string{
		"alpha": {providers.AuthOAuth, "https://alpha.example.cloud.databricks.com", ""},
		"beta":  {providers.AuthM2M, "https://beta.example.cloud.databricks.com", ""},
		"gamma": {providers.AuthPAT, "https://gamma.example.cloud.databricks.com", ""},
	}
	for n, w := range want {
		if c := got[n]; c.Auth != w[0] || c.Host != w[1] {
			t.Errorf("%s = %+v", n, c)
		}
	}
}

// checkNoSecretInCandidates fails when any Candidate field holds a secret value.
func checkNoSecretInCandidates(t *testing.T, cs []Candidate) {
	t.Helper()
	for _, c := range cs {
		if strings.Contains(strings.ToLower(reflect.ValueOf(c).String()+c.Host+c.ClientID), "shh") {
			t.Errorf("secret in candidate %+v", c)
		}
	}
}

func TestReadSecrets(t *testing.T) {
	p := writeFixtureCfg(t)
	s, err := ReadSecrets(p, "beta")
	if err != nil || s.ClientSecret != "shh-beta" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := ReadSecrets(p, "DEFAULT"); err == nil {
		t.Fatal("DEFAULT must not be readable")
	}
}

func TestPlan(t *testing.T) {
	cs, _ := Candidates(writeFixtureCfg(t))
	existing := map[string]json.RawMessage{
		"alpha": json.RawMessage(`{"auth":{"type":"oauth"}}`),
		"beta":  json.RawMessage(`{"auth":{"type":"pat"}}`),
	}
	byID := func(o ImportOptions) map[string]Row {
		m := map[string]Row{}
		for _, r := range ImportPlan(cs, existing, o) {
			m[r.Name] = r
		}
		return m
	}
	m := byID(ImportOptions{Rename: map[string]string{"gamma": "gamma2", "bad name": "fixed"}})
	if m["alpha"].Action != "skip" || m["gamma"].ID != "gamma2" || m["gamma"].Action != "add" ||
		m["bad name"].Action != "add" || m["delta"].Action != "skip" {
		t.Errorf("plan: %+v", m)
	}
	if m := byID(ImportOptions{Replace: true}); m["alpha"].Action != "replace" ||
		!strings.Contains(m["beta"].Reason, "never changed") || m["bad name"].Action != "skip" {
		t.Errorf("replace plan: %+v", m)
	}
}

func TestApplyAndSecretName(t *testing.T) {
	top := map[string]json.RawMessage{"default": json.RawMessage(`"x"`)}
	rows := []Row{{Host: "https://h.example.com", Auth: providers.AuthM2M, ClientID: "c", ID: "my-sp", Action: "add"}}
	if err := Apply(top, rows); err != nil {
		t.Fatal(err)
	}
	if string(top["default"]) != `"x"` || !strings.Contains(string(top["providers"]), `"clientId":"c"`) {
		t.Errorf("top = %v", top)
	}
	if n := SecretNameOf(rows[0]); n != "DATABRICKS_MY_SP_M2M" {
		t.Errorf("name %s", n)
	}
	if strings.Contains(string(top["providers"]), "secret") && strings.Contains(string(top["providers"]), "shh") {
		t.Error("secret in row")
	}
}

const orderCfg = `[prod]
host = https://prod.cloud.databricks.com
token = dapi1

[dev]
host = https://dev.cloud.databricks.com
token = dapi2

[qa]
host = https://qa.cloud.databricks.com
token = dapi3
`

func TestImportFollowsSectionOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(orderCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	if want := []string{"prod", "dev", "qa"}; !slices.Equal(names, want) {
		t.Fatalf("Candidates = %v, want section order %v", names, want)
	}
	top := map[string]json.RawMessage{}
	if err := Apply(top, ImportPlan(cs, Existing(top), ImportOptions{})); err != nil {
		t.Fatal(err)
	}
	if got, want := providers.ObjectKeys(top["providers"]), []string{"prod", "dev", "qa"}; !slices.Equal(got, want) {
		t.Errorf("Apply order = %v, want %v (not sorted)", got, want)
	}
}

func TestApplyKeepsExistingRowsInPlaceAndAppendsNew(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(orderCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	ex, _ := providers.MarshalOrdered([]string{"qa", "legacy"}, map[string]json.RawMessage{
		"qa":     json.RawMessage(`{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}`),
		"legacy": json.RawMessage(`{"preset":"databricks","host":"https://l.cloud.databricks.com"}`),
	})
	top := map[string]json.RawMessage{"providers": ex}
	if err := Apply(top, ImportPlan(cs, Existing(top), ImportOptions{Replace: true})); err != nil {
		t.Fatal(err)
	}
	if got, want := providers.ObjectKeys(top["providers"]), []string{"qa", "legacy", "prod", "dev"}; !slices.Equal(got, want) {
		t.Errorf("Apply order = %v, want %v", got, want)
	}
}

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
}

func TestPairingCommentSurvivesMerge(t *testing.T) {
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
