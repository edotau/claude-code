package profile

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// List prints the set names, marking the active one.
func List(w io.Writer, reg *providers.Registry) {
	if len(reg.Profiles.Sets) == 0 {
		fmt.Fprintln(w, "no profile sets (none active: launches use today's selection)")
		return
	}
	for _, n := range slices.Sorted(maps.Keys(reg.Profiles.Sets)) {
		mark := " "
		if n == reg.Profiles.Active {
			mark = "*"
		}
		fmt.Fprintf(w, "%s %s\n", mark, n)
	}
	if reg.Profiles.Active == "" {
		fmt.Fprintln(w, "(none active)")
	}
}

// dialectOf is the wire dialect each profile agent talks to its account in.
func dialectOf(agent string) string {
	switch agent {
	case "session", "gemini":
		return providers.DialectAnthropic
	case "codex":
		return providers.DialectResponses
	}
	return providers.DialectOpenAI
}

// Show prints name's per-agent table and, for the active set, any env drift against its session row.
func Show(w io.Writer, reg *providers.Registry, name string) error {
	set, ok := reg.Profiles.Sets[name]
	if !ok {
		return fmt.Errorf("no profile set %q (see `claude-code profile list`)", name)
	}
	state := ""
	if name == reg.Profiles.Active {
		state = " (active)"
	}
	fmt.Fprintf(w, "profile %s%s\n", name, state)
	if len(set) == 0 {
		fmt.Fprintln(w, "  (empty: every agent keeps today's selection)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  AGENT\tACCOUNT\tMODE\tHOST\tMODEL\tCREDENTIAL RESOLVES")
		for _, a := range providers.ProfileAgents {
			if row, ok := set[a]; ok {
				fmt.Fprintln(tw, "  "+tableRow(reg, a, row))
			}
		}
		tw.Flush()
	}
	if state != "" {
		for _, d := range Drift(reg) {
			fmt.Fprintln(w, "drift: "+d)
		}
	}
	return nil
}

func tableRow(reg *providers.Registry, agent string, row providers.AgentRow) string {
	account, mode, host, cred := cmp.Or(row.Account, "-"), "-", "-", "-"
	if p, ok := reg.Providers[row.Account]; ok {
		mode = "apikey"
		if p.Auth.Type == providers.AuthPassthrough {
			mode = "subscription"
		}
		host = hostOf(p, dialectOf(agent))
		cred = "no"
		if providers.CredentialPresent(p) {
			cred = "yes"
		}
	}
	model := cmp.Or(row.Model, "-")
	if len(row.Models) > 0 {
		var parts []string
		for _, s := range models.Slots {
			if m := row.Models[s]; m != "" {
				parts = append(parts, s+"="+m)
			}
		}
		model = strings.Join(parts, " ")
	}
	return strings.Join([]string{agent, account, mode, host, model, cred}, "\t")
}

// hostOf is the host serving dialect (falling back to the provider's base URL host).
func hostOf(p *providers.Provider, dialect string) string {
	base, ok := p.Route(dialect)
	if !ok {
		base = p.BaseURL
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return cmp.Or(base, "-")
}

// Drift reports process env (HARNESS_PROVIDER, HARNESS_MODEL, ANTHROPIC_BASE_URL) contradicting the active session row.
func Drift(reg *providers.Registry) []string {
	row, ok := reg.AgentRow("session")
	if !ok || row.Account == "" {
		return nil
	}
	var out []string
	if v := os.Getenv(providers.PinProvider); v != "" && v != row.Account {
		out = append(out, fmt.Sprintf("%s=%s but the session account is %s", providers.PinProvider, v, row.Account))
	}
	if v := os.Getenv(providers.PinModel); v != "" && len(row.Models) > 0 && !hasModel(row.Models, v) {
		out = append(out, fmt.Sprintf("%s=%s is not one of the session models", providers.PinModel, v))
	}
	p, found := reg.Providers[row.Account]
	if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" && found {
		if u, err := url.Parse(v); err != nil || u.Host != hostOf(p, providers.DialectAnthropic) {
			out = append(out, fmt.Sprintf("ANTHROPIC_BASE_URL host differs from %s (%s)", row.Account,
				hostOf(p, providers.DialectAnthropic)))
		}
	}
	return out
}

func hasModel(slots map[string]string, v string) bool {
	v = models.Strip1M(v)
	for _, m := range slots {
		if models.Strip1M(m) == v {
			return true
		}
	}
	return false
}
