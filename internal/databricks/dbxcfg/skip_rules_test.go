package dbxcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

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
	if c := got["ids"]; c.Auth != providers.AuthU2M || c.AccountID != "1234abcd-0000" || c.WorkspaceID != "42" {
		t.Errorf("ids = %+v", c)
	}
}
