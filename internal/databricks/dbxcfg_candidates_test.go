package databricks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

var secretish = regexp.MustCompile(`(?i)token|secret|password|passwd|pat$|bearer|credential|apikey|key`)

// Candidate must be structurally unable to carry a secret: no secret-named string field, only presence flags.
func TestCandidateHasNoSecretBearingField(t *testing.T) {
	typ := reflect.TypeFor[Candidate]()
	for f := range typ.Fields() {
		if secretish.MatchString(f.Name) && f.Type.Kind() != reflect.Bool {
			t.Errorf("Candidate.%s (%s) looks secret-bearing", f.Name, f.Type)
		}
		switch f.Type.Kind() {
		case reflect.String, reflect.Bool:
		default:
			t.Errorf("Candidate.%s has kind %s; only string/bool fields are allowed", f.Name, f.Type.Kind())
		}
	}
}

func TestSecretValuesNeverAppearInCandidatesOrPlan(t *testing.T) {
	cs, err := Candidates(writeFixtureCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	rows := ImportPlan(cs, nil, ImportOptions{})
	var sb strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&sb, "%+v %#v\n", c, c)
		b, _ := json.Marshal(c)
		sb.Write(b)
	}
	for _, r := range rows {
		fmt.Fprintf(&sb, "%+v\n", r)
		if b, err := RowJSON(r); err == nil {
			sb.Write(b)
		}
	}
	for _, leak := range []string{"shh-beta", "dapi0123", "dapiDEFAULTLEAK", "leak.example"} {
		if strings.Contains(sb.String(), leak) {
			t.Errorf("%q leaked into candidates/plan output", leak)
		}
	}
}

func TestDefaultAndSettingsSectionsAreNeverCandidates(t *testing.T) {
	cs, err := Candidates(writeFixtureCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if strings.EqualFold(c.Name, "default") || c.Name == "__settings__" {
			t.Errorf("section %q became a candidate", c.Name)
		}
	}
	if _, err := ReadSecrets(writeFixtureCfg(t), "__settings__"); err == nil {
		t.Error("__settings__ is readable as a secret source")
	}
}

func candidatesOf(t *testing.T, cfg string) []Candidate {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestHeaderlessKeysBelongToDefault(t *testing.T) {
	cs := candidatesOf(t, "host = https://x.cloud.databricks.com\ntoken = t\n")
	if len(cs) != 1 || cs[0].Name != "DEFAULT" || cs[0].Skip != defaultOnlySkip {
		t.Fatalf("headerless cfg: want the DEFAULT skip row, got %+v", cs)
	}
}

func TestEmptyDefaultHeaderIsNoDefault(t *testing.T) {
	if cs := candidatesOf(t, "[DEFAULT]\n"); len(cs) != 0 {
		t.Fatalf("empty [DEFAULT] must not yield a skip row, got %+v", cs)
	}
}

func TestUnsupportedAndIncompleteProfilesSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg")
	cfg := "[azure]\nhost = https://a.example.cloud.databricks.com\nauth_type = azure-cli\n" +
		"[m2m-nocid]\nhost = https://b.example.cloud.databricks.com\nauth_type = oauth-m2m\n" +
		"[ids]\nhost = https://c.example.cloud.databricks.com\nauth_type = databricks-cli\naccount_id = 1234abcd-0000\nworkspace_id = 42\n"
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Candidate{}
	for _, c := range cs {
		got[c.Name] = c
	}
	if c := got["azure"]; c.Auth != "" || !strings.Contains(c.Skip, "unsupported auth_type") {
		t.Errorf("azure = %+v", c)
	}
	if c := got["m2m-nocid"]; c.Auth != "" || c.Skip != "oauth-m2m profile without client_id" {
		t.Errorf("m2m-nocid = %+v", c)
	}
	if c := got["ids"]; c.Auth != providers.AuthOAuth || c.AccountID != "1234abcd-0000" || c.WorkspaceID != "42" {
		t.Errorf("ids = %+v", c)
	}
}

func candsOf(t *testing.T, body string) map[string]Candidate {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Candidate{}
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

func TestParseInlineCommentsStripped(t *testing.T) {
	m := candsOf(t, "[a]\nhost = https://a.example.cloud.databricks.com # prod ws\nclient_id = cid # id\nclient_secret = s ; sec\n")
	c := m["a"]
	if c.Host != "https://a.example.cloud.databricks.com" || c.ClientID != "cid" || c.Auth != providers.AuthM2M {
		t.Errorf("a = %+v", c)
	}
}

func TestParseQuotedValues(t *testing.T) {
	m := candsOf(t, "[q]\nhost = \"https://q.example.cloud.databricks.com\"\nclient_id = 'cid-q'\nclient_secret = \"s\"\n")
	if c := m["q"]; c.Host != "https://q.example.cloud.databricks.com" || c.ClientID != "cid-q" || c.Skip != "" {
		t.Errorf("q = %+v", c)
	}
}

func TestParseHeaderWithTrailingComment(t *testing.T) {
	m := candsOf(t, "[x] # work\nhost = https://x.example.cloud.databricks.com\n")
	if c, ok := m["x"]; !ok || c.Host == "" {
		t.Errorf("header with trailing comment lost: %+v", m)
	}
}

func TestParseDuplicateKeyLastWins(t *testing.T) {
	m := candsOf(t, "[d]\nhost = https://old.example.cloud.databricks.com\nhost = https://new.example.cloud.databricks.com\n")
	if got := m["d"].Host; got != "https://new.example.cloud.databricks.com" {
		t.Errorf("host = %q, want the last", got)
	}
}

func TestParseDuplicateSectionMergesLastWins(t *testing.T) {
	m := candsOf(t, "[d]\nhost = https://old.example.cloud.databricks.com\n[e]\nhost = https://e.example.cloud.databricks.com\n"+
		"[d]\nhost = https://new.example.cloud.databricks.com\nclient_id = c\n")
	if got := m["d"]; got.Host != "https://new.example.cloud.databricks.com" || got.ClientID != "c" {
		t.Errorf("d = %+v", got)
	}
}

func TestDefaultOnlyFileReason(t *testing.T) {
	m := candsOf(t, "[DEFAULT]\nhost = https://d.example.cloud.databricks.com\ntoken = dapi-default\n")
	c, ok := m["DEFAULT"]
	if !ok || c.Skip != defaultOnlySkip || c.Auth != "" {
		t.Errorf("DEFAULT-only file: %+v", m)
	}
	if strings.Contains(c.Skip+c.Host, "dapi-default") {
		t.Error("DEFAULT value leaked into the candidate")
	}
}

func TestExplicitAuthTypeWinsOverInference(t *testing.T) {
	m := candsOf(t, "[p]\nhost = https://p.example.cloud.databricks.com\nauth_type = pat\ntoken = t\nclient_id = c\nclient_secret = s\n"+
		"[u]\nhost = https://u.example.cloud.databricks.com\nauth_type = databricks-cli\ntoken = t\n")
	if m["p"].Auth != providers.AuthPAT {
		t.Errorf("explicit pat lost to inferred m2m: %+v", m["p"])
	}
	if c := m["u"]; c.Auth != providers.AuthOAuth || c.HasSecret {
		t.Errorf("explicit databricks-cli must map to u2m with no secret: %+v", c)
	}
}

func TestParseMultiLineValues(t *testing.T) {
	body := "[m]\ntoken = \"\"\"ab\ncd\"\"\"\nclient_secret = x\\\n  y\nclient_id = `p\nq`\nhost = h # c\n"
	ps, _, err := parse(strings.NewReader(body))
	if err != nil || len(ps) != 1 {
		t.Fatalf("parse = %v, %v", ps, err)
	}
	p := ps[0]
	if p.token != "ab\ncd" || p.clientSecret != "xy" || p.clientID != "p\nq" || p.host != "h" {
		t.Errorf("profile = %+v", p)
	}
}

func patCand(name string) Candidate {
	return Candidate{Name: name, Host: "https://example.cloud.databricks.com", Auth: providers.AuthPAT, HasSecret: true}
}

func TestPlanSkipsCollidingSecretNames(t *testing.T) {
	rows := ImportPlan([]Candidate{patCand("a-b"), patCand("a_b")}, nil, ImportOptions{})
	if rows[0].Action != "add" {
		t.Errorf("first = %+v", rows[0])
	}
	if rows[1].Action != "skip" || rows[1].Reason == "" {
		t.Errorf("second should be skipped with a reason: %+v", rows[1])
	}
}

func TestPlanReplaceOfNoAuthTypeRowSkipsOnlyThatRow(t *testing.T) {
	existing := map[string]json.RawMessage{
		"keep": json.RawMessage(`{"preset":"databricks","host":"https://example.cloud.databricks.com","auth":{"type":"key"}}`),
	}
	rows := ImportPlan([]Candidate{patCand("keep"), patCand("fresh")}, existing, ImportOptions{Replace: true})
	if rows[0].Action != "skip" || !strings.Contains(rows[0].Reason, "databricks login --profile keep --auth-type oauth") {
		t.Errorf("existing = %+v", rows[0])
	}
	if rows[1].Action != "add" {
		t.Errorf("fresh = %+v", rows[1])
	}
	top := map[string]json.RawMessage{}
	if err := Apply(top, rows); err != nil {
		t.Fatal(err)
	}
	if _, ok := Existing(top)["fresh"]; !ok {
		t.Error("fresh row not applied")
	}
}
