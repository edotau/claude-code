package vaultdoc

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// ProviderEntry is one provider in the JSON listing: its shape and where its credential resolves from, never the value.
type ProviderEntry struct {
	Name        string            `json:"name"`
	Protocol    string            `json:"protocol"`
	Dialects    []string          `json:"dialects"`
	Auth        string            `json:"auth"`
	Login       string            `json:"login,omitempty"`
	Credential  bool              `json:"credential"`
	Models      map[string]string `json:"models,omitempty"`
	Group       string            `json:"group"` // the family the summary files it under (anthropic, openai, ... local)
	Source      string            `json:"source"`
	Note        string            `json:"note"`
	Host        string            `json:"host,omitempty"` // non-secret: preset host, else BaseURL's scheme+host
	AccountID   string            `json:"accountId,omitempty"`
	WorkspaceID string            `json:"workspaceId,omitempty"`
	Options     providers.Options `json:"options,omitzero"`
}

// NewProviderEntry builds p's listing entry; p.Name is its registry key.
func NewProviderEntry(p *providers.Provider) ProviderEntry {
	e := ProviderEntry{
		Name: p.Name, Protocol: p.Protocol, Dialects: p.Dialects(), Auth: p.Auth.Method(), Login: p.Auth.Login,
		Credential: providers.CredentialPresent(p), Models: p.Models, Group: p.Provider,
		Host: providers.AuthHost(p), AccountID: p.Auth.AccountID, WorkspaceID: p.Auth.WorkspaceID, Options: p.Options,
	}
	e.Source, e.Note = providers.CredSource(p)
	return e
}

// ProviderList is the whole JSON listing, the shape `vault ls providers --format json` prints.
type ProviderList struct {
	Session  string          `json:"session"`
	Default  string          `json:"default"`
	Fallback []string        `json:"fallback,omitempty"`
	List     []ProviderEntry `json:"list"`
}

// ProviderRows is every provider, or the one named; an unknown name is an error.
func ProviderRows(reg *providers.Registry, name string) (ProviderList, error) {
	if name != "" {
		p, err := reg.Get(name)
		if err != nil {
			return ProviderList{}, err
		}
		return ProviderList{List: []ProviderEntry{NewProviderEntry(p)}}, nil
	}
	names := reg.Names()
	list := ProviderList{Default: reg.Default, Fallback: reg.Fallback, List: make([]ProviderEntry, 0, len(names))}
	for _, n := range names {
		list.List = append(list.List, NewProviderEntry(reg.Providers[n]))
	}
	if sel, err := providers.Select(reg, ""); err == nil && sel.Provider != nil {
		list.Session = sel.Provider.Name
	}
	return list, nil
}

// ProviderJSON is the value `vault ls providers [<name>] --format json` encodes: the list, or the one entry.
func ProviderJSON(reg *providers.Registry, name string) (any, error) {
	rows, err := ProviderRows(reg, name)
	if err != nil {
		return nil, err
	}
	if name != "" {
		return rows.List[0], nil
	}
	return rows, nil
}

// ProviderLines is the text view: the summary of profiles whose credential resolves, or one provider's detail.
func ProviderLines(reg *providers.Registry, name string) ([]string, error) {
	if name != "" {
		p, err := reg.Get(name)
		if err != nil {
			return nil, err
		}
		source, note := providers.CredSource(p)
		return providers.ProfileDetail(p, source, note), nil
	}
	var agents []providers.AgentServer
	for _, v := range agent.Names() {
		agents = append(agents, providers.AgentServer{Name: v, Serves: func(p *providers.Provider) bool { return agent.CanServe(v, p) }})
	}
	ready := func(p *providers.Provider) bool { src, _ := providers.CredSource(p); return src != "missing" }
	return providers.ListTable(reg, ready, agents), nil
}
