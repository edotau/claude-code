package cli

import (
	"fmt"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// printLines prints each line of a rendered view.
func printLines(lines []string) {
	for _, l := range lines {
		fmt.Println(l)
	}
}

// configureProviderList prints every provider's credential state, or one provider's detail; json mirrors describe.
func configureProviderList(name, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("configure provider", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if format == helptext.JSON {
		return providerListJSON(reg, name)
	}
	if name == "" {
		printProviderTable(reg)
		return 0
	}
	p, err := reg.Get(name)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	source, note := providers.CredSource(p)
	printLines(providers.ProfileDetail(p, source, note))
	return 0
}

// printProviderTable prints the provider and agent-CLI summary of the profiles whose credential resolves now.
func printProviderTable(reg *providers.Registry) {
	var agents []providers.AgentServer
	for _, v := range agent.Names() {
		agents = append(agents, providers.AgentServer{Name: v, Serves: func(p *providers.Provider) bool { return agent.CanServe(v, p) }})
	}
	ready := func(p *providers.Provider) bool { src, _ := providers.CredSource(p); return src != "missing" }
	printLines(providers.ListTable(reg, ready, agents))
}

// providerListEntry is the describe provider view plus where its credential resolves from.
type providerListEntry struct {
	DescribeProvider
	Group       string `json:"group"` // the family `--list` files it under (anthropic, openai, ... local)
	Source      string `json:"source"`
	Note        string `json:"note"`
	Host        string `json:"host,omitempty"` // non-secret: preset host, else BaseURL's scheme+host
	AccountID   string `json:"accountId,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

// providerListJSON emits the describe provider view: all providers, or the one named, never a credential.
func providerListJSON(reg *providers.Registry, name string) int {
	d, err := describe(reg)
	if err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	var want *providers.Provider
	if name != "" {
		if want, err = reg.Get(name); err != nil {
			return helptext.Fail("%v", err)
		}
	}
	list := make([]providerListEntry, 0, len(d.Providers.List))
	for _, dp := range d.Providers.List {
		p := reg.Providers[dp.Name]
		if want != nil && p != want {
			continue
		}
		e := providerListEntry{DescribeProvider: dp}
		e.Group = p.Provider
		e.Source, e.Note = providers.CredSource(p)
		e.Host, e.AccountID, e.WorkspaceID = providers.AuthHost(p), p.Auth.AccountID, p.Auth.WorkspaceID
		list = append(list, e)
	}
	if want != nil {
		if len(list) == 0 {
			return helptext.Fail("configure provider: %q not in the provider list", name)
		}
		return helptext.WriteJSONOrFail("configure provider", list[0])
	}
	return helptext.WriteJSONOrFail("configure provider", struct {
		Session  string              `json:"session"`
		Default  string              `json:"default"`
		Fallback []string            `json:"fallback,omitempty"`
		List     []providerListEntry `json:"list"`
	}{d.Providers.Session, d.Providers.Default, d.Providers.Fallback, list})
}
