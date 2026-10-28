package dbxcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func writeCfg(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCandidates(t *testing.T) {
	cs, err := Candidates(writeCfg(t))
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
	if got["delta"].Skip == "" || got["beta"].ClientID != "cid-beta" || !got["beta"].HasSecret {
		t.Errorf("delta/beta: %+v %+v", got["delta"], got["beta"])
	}
	// No Candidate field may hold a secret value.
	for _, c := range cs {
		if strings.Contains(strings.ToLower(reflect.ValueOf(c).String()+c.Host+c.ClientID), "shh") {
			t.Errorf("secret in candidate %+v", c)
		}
	}
}

func TestReadSecrets(t *testing.T) {
	p := writeCfg(t)
	s, err := ReadSecrets(p, "beta")
	if err != nil || s.ClientSecret != "shh-beta" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := ReadSecrets(p, "DEFAULT"); err == nil {
		t.Fatal("DEFAULT must not be readable")
	}
}

func TestPlan(t *testing.T) {
	cs, _ := Candidates(writeCfg(t))
	existing := map[string]json.RawMessage{
		"alpha": json.RawMessage(`{"auth":{"type":"oauth"}}`),
		"beta":  json.RawMessage(`{"auth":{"type":"pat"}}`),
	}
	byID := func(o Options) map[string]Row {
		m := map[string]Row{}
		for _, r := range Plan(cs, existing, o) {
			m[r.Name] = r
		}
		return m
	}
	m := byID(Options{Rename: map[string]string{"gamma": "gamma2", "bad name": "fixed"}})
	if m["alpha"].Action != "skip" || m["gamma"].ID != "gamma2" || m["gamma"].Action != "add" ||
		m["bad name"].Action != "add" || m["delta"].Action != "skip" {
		t.Errorf("plan: %+v", m)
	}
	if m := byID(Options{Replace: true}); m["alpha"].Action != "replace" ||
		!strings.Contains(m["beta"].Reason, "never changed") || m["bad name"].Action != "skip" {
		t.Errorf("replace plan: %+v", m)
	}
}

func TestApplyAndSecretName(t *testing.T) {
	top := map[string]json.RawMessage{"default": json.RawMessage(`"x"`)}
	rows := []Row{{Candidate: Candidate{Host: "https://h.example.com", Auth: providers.AuthM2M, ClientID: "c"}, ID: "my-sp", Action: "add"}}
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
