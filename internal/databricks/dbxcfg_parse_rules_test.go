package databricks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

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
