package proxy

import "github.com/edotau/claude-code/internal/providers"

// authHint names the remedy a rejected credential needs, picked by the provider's auth type.
func authHint(p *providers.Provider) string {
	a := p.Auth
	switch {
	case p.Preset == providers.PresetDatabricks || a.Type == providers.AuthOAuth || a.Type == providers.AuthM2M:
		return "run `claude-code databricks login --profile " + p.Name + "`"
	case a.Command != "":
		return "check the provider's auth.command"
	case (a.Type == providers.AuthKey || a.Type == providers.AuthPAT) && a.Env != "":
		return "run `claude-code vault set " + a.Env + "`"
	}
	return "run `claude-code configure provider --list " + p.Name + "`"
}
