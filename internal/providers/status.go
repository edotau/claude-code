package providers

import (
	"github.com/edotau/claude-code/internal/vault"
)

// ProviderStatus is one provider's credential state: which source would serve it today, never the
// value itself.
type ProviderStatus struct {
	Name   string
	Type   string
	Source string // env | vault | auth.file | command | cli | none
	Note   string
}

// Resolve reports credential presence in env -> vault -> auth.file -> command order, without resolving a key.
func Resolve(p *Provider) ProviderStatus {
	st := ProviderStatus{Name: p.Name, Type: p.Auth.Type}
	switch p.Auth.Type {
	case AuthNone:
		st.Source = "none"
		st.Note = "no credential needed"
		return st
	case AuthCLI:
		st.Source = "cli"
		if p.Auth.Login != "" {
			st.Note = "via `" + p.Auth.Login + "` login"
		}
		return st
	}
	if p.Auth.Env != "" {
		source, err := vault.HasSecretErr(p.Auth.Env)
		st.Source = source
		if err != nil {
			st.Note = "vault unreadable: " + err.Error()
		}
	}
	if st.Source == "" {
		st.Source = fallbackSource(p)
	}
	if st.Source == "none" && p.Auth.Env != "" && st.Note == "" {
		st.Note = "set " + p.Auth.Env
	}
	return st
}

// fallbackSource names the auth.file or command source when the vault has none, else "none".
func fallbackSource(p *Provider) string {
	switch {
	case fileCredential(p) != "":
		return "auth.file"
	case p.Auth.Command != "":
		return "command"
	}
	return "none"
}
