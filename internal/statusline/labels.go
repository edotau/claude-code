package statusline

import "github.com/edotau/claude-code/internal/providers"

// labeler holds the per-kind cells: the provider label, the model cell and the auth cell.
type labeler struct {
	provider func(p *providers.Provider) string
	model    func(in Input, session string) string
	auth     func(in Input, session string, p *providers.Provider) string
}

var genericLabeler = labeler{
	provider: func(p *providers.Provider) string { return kindLabel(p, p.Name) },
	model:    modelSegment,
	auth:     tokenAuth,
}

// labelerFor picks the labeler by the registry row's kind; no row or an unknown kind is generic.
func labelerFor(p *providers.Provider) labeler {
	if p != nil {
		switch p.Provider {
		case "anthropic":
			return anthropicLabeler
		case "databricks":
			return databricksLabeler
		}
	}
	return genericLabeler
}

// rowOf is the session's registry row, nil when the registry or row is missing.
func rowOf(session string) *providers.Provider {
	if reg := registry(); reg != nil {
		return reg.Providers[session]
	}
	return nil
}

// kindLabel renders "<provider>·<profile>", the provider alone when the profile bears its name.
func kindLabel(p *providers.Provider, profile string) string {
	if p.Name == p.Provider {
		return p.Provider
	}
	return p.Provider + "·" + profile
}
