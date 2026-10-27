package providers

import (
	"net/url"
	"strings"
)

// GroupDatabricks is the family of every Databricks preset or sealed-auth row.
const GroupDatabricks = "databricks"

// Group files p under a family: Databricks by preset or sealed auth, the rest by its route's host.
func Group(p *Provider) string {
	if p.Preset == PresetDatabricks || p.Auth.AuthType != "" {
		return GroupDatabricks
	}
	host := ""
	for _, d := range p.Dialects() {
		if r, ok := p.Route(d); ok {
			if u, err := url.Parse(r); err == nil {
				host = u.Hostname()
			}
			break
		}
	}
	for _, m := range []struct{ suffix, group string }{
		{"anthropic.com", "anthropic"}, {"openai.com", "openai"}, {"chatgpt.com", "openai"},
		{"githubcopilot.com", "github"}, {"github.com", "github"}, {"googleapis.com", "google"},
		{"databricks.com", GroupDatabricks},
	} {
		if host == m.suffix || strings.HasSuffix(host, "."+m.suffix) {
			return m.group
		}
	}
	return "opensource"
}
