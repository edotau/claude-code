package providers

import (
	"strings"
	"testing"
)

func TestRetiredAuthTypesRejected(t *testing.T) {
	for _, old := range []string{"databricks-cli", "m2m", "cli-profile", "external-browser", "SAML"} {
		errs := strings.Join(validateAuthType("x", kindRow(old, "", AuthBearer)), ";")
		want := `authType "` + old + `" not supported (supported: u2m, oauth-m2m, pat)`
		if !strings.Contains(errs, want) {
			t.Errorf("%s: errs %q, want %q", old, errs, want)
		}
	}
}

func TestAccountAndWorkspaceIDValidation(t *testing.T) {
	cases := []struct{ acct, ws, want string }{
		{"", "", ""},
		{"1234abcd-0000-ffff", "1234567890", ""},
		{"short", "", "auth.accountId"},
		{"zzzzzzzzzz", "", "auth.accountId"},
		{"", "12ab", "auth.workspaceId"},
	}
	for _, c := range cases {
		p := kindRow("", "", AuthBearer)
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
