package providers

import (
	"strings"
	"testing"
)

func TestAccountAndWorkspaceIDValidation(t *testing.T) {
	cases := []struct{ acct, ws, want string }{
		{"", "", ""},
		{"1234abcd-0000-ffff", "1234567890", ""},
		{"short", "", "auth.accountId"},
		{"zzzzzzzzzz", "", "auth.accountId"},
		{"", "12ab", "auth.workspaceId"},
	}
	for _, c := range cases {
		p := kindRow("", "", AuthKey)
		p.Auth.AccountID, p.Auth.WorkspaceID = c.acct, c.ws
		errs := strings.Join(validateAuthType("x", p), ";")
		if c.want == "" && errs != "" || c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("acct=%q ws=%q: errs %q, want %q", c.acct, c.ws, errs, c.want)
		}
	}
}

func TestValidIDShape(t *testing.T) {
	for id, want := range map[string]bool{"my-sp_1": true, "": false, "bad name": false, "a/b": false} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, !want, want)
		}
	}
}

func TestClientIDInjectionRejected(t *testing.T) {
	for _, id := range []string{"a\nb", "a\rb", "a[b", "a=b"} {
		p := kindRow(AuthM2M, id, AuthKey)
		if errs := strings.Join(validateAuthType("x", p), ";"); !strings.Contains(errs, "auth.clientId must not contain") {
			t.Errorf("clientId %q: errs %q", id, errs)
		}
	}
	if errs := validateAuthType("x", kindRow(AuthM2M, "sp-client", AuthKey)); len(errs) != 0 {
		t.Errorf("clean clientId rejected: %v", errs)
	}
}

// An OAuth provider's missing-credential hint names both the databricks login and the configure Set up route.
func TestAuthHintOAuthNamesLoginAndSetUp(t *testing.T) {
	p := kindRow(AuthOAuth, "", "")
	p.Name = "dbx"
	got := authHint(p, "DBX_TOKEN")
	for _, want := range []string{"claude-code databricks login --profile dbx", "claude-code configure", "Set up"} {
		if !strings.Contains(got, want) {
			t.Errorf("OAuth hint %q lacks %q", got, want)
		}
	}
	if key := authHint(kindRow(AuthKey, "", ""), "DBX_TOKEN"); !strings.Contains(key, "vault set") || strings.Contains(key, "databricks login") {
		t.Errorf("key-auth hint = %q, want the vault set route only", key)
	}
}
