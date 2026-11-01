package server

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestAuthHintByAuthType(t *testing.T) {
	cases := map[string]struct {
		p    providers.Provider
		want string
	}{
		"oauth":   {providers.Provider{Name: "d", Auth: providers.Auth{Type: providers.AuthOAuth}}, "claude-code databricks login --profile d"},
		"preset":  {providers.Provider{Name: "d", Preset: providers.PresetDatabricks}, "claude-code databricks login --profile d"},
		"dbx pat": {providers.Provider{Name: "d", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: providers.AuthPAT}}, "--profile d --auth-type oauth"},
		"key":     {providers.Provider{Name: "k", Auth: providers.Auth{Type: providers.AuthKey, Env: "K_KEY"}}, "claude-code vault set K_KEY"},
		"command": {providers.Provider{Name: "c", Auth: providers.Auth{Type: providers.AuthKey, Command: "op read x"}}, "auth.command"},
		"other":   {providers.Provider{Name: "o", Auth: providers.Auth{Type: providers.AuthCLI}}, "claude-code configure provider --list o"},
	}
	for name, c := range cases {
		if got := authHint(&c.p); !strings.Contains(got, c.want) {
			t.Errorf("%s: authHint = %q, want %q", name, got, c.want)
		}
	}
}
