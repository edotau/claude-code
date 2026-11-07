package providers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault"
)

// providerGroups is the family order `configure provider --list` prints; local takes every other host.
var providerGroups = []string{"anthropic", "openai", "github", "google", GroupDatabricks, "nvidia", "local"}

// AgentServer is an agent CLI and whether it can run a profile; ListTable files one row per agent.
type AgentServer struct {
	Name   string
	Serves func(*Provider) bool
}

// ListTable renders the `configure provider --list` summary: one row per provider family, then one per agent CLI,
// over the profiles whose credential is ready (m2m service principals excluded), then the fallback chain.
func ListTable(reg *Registry, ready func(*Provider) bool, agents []AgentServer) []string {
	groups, modes := readyGroups(reg, ready)
	rows, live := providerRows(reg, groups, modes)
	rows = append(rows, [3]string{})
	rows = append(rows, agentRows(reg, agents, live)...)
	w0, w1 := len("PROVIDERS"), len("PROFILES")
	for _, r := range rows {
		w0, w1 = max(w0, len(r[0])), max(w1, len(r[1]))
	}
	lines := []string{fmt.Sprintf("%-*s  %-*s  %s", w0, "PROVIDERS", w1, "PROFILES", "AUTH")}
	for _, r := range rows {
		lines = append(lines, strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %s", w0, r[0], w1, r[1], r[2]), " "))
	}
	if len(reg.Fallback) > 0 {
		lines = append(lines, "", "fallback: "+strings.Join(reg.Fallback, " → "))
	}
	return lines
}

// readyGroups files the ready profile names and auth modes under their provider family.
func readyGroups(reg *Registry, ready func(*Provider) bool) (groups, modes map[string][]string) {
	groups, modes = map[string][]string{}, map[string][]string{}
	for _, n := range reg.Names() {
		p := reg.Providers[n]
		if !ready(p) {
			continue
		}
		g := p.Provider
		modes[g] = appendUnique(modes[g], providerMode(p))
		if p.Auth.Method() == AuthM2M {
			continue // a service principal mints oauth logins from a cold start; it is an auth, not a profile
		}
		groups[g] = append(groups[g], n)
	}
	return groups, modes
}

// providerRows is one row per non-empty family, plus the live profiles in provider order.
func providerRows(reg *Registry, groups, modes map[string][]string) (rows [][3]string, live []*Provider) {
	for _, g := range providerGroups {
		if len(groups[g]) == 0 {
			continue
		}
		names := sortProfiles(groups[g])
		rows = append(rows, [3]string{g, familyProfiles(reg, names), strings.Join(modes[g], ", ")})
		for _, n := range names {
			live = append(live, reg.Providers[n])
		}
	}
	return rows, live
}

// agentRows is one row per agent CLI that serves at least one live profile.
func agentRows(reg *Registry, agents []AgentServer, live []*Provider) [][3]string {
	var rows [][3]string
	for _, a := range agents {
		var names, vm []string
		for _, p := range live {
			if a.Serves(p) {
				names, vm = append(names, p.Name), appendUnique(vm, providerMode(p))
			}
		}
		if len(names) > 0 {
			rows = append(rows, [3]string{a.Name, familyProfiles(reg, names), strings.Join(vm, ", ")})
		}
	}
	return rows
}

// ProfileDetail renders one provider's `--list <name>` view; source and note are its resolved credential origin.
func ProfileDetail(p *Provider, source, note string) []string {
	auth := p.Auth.Method()
	if p.Auth.Env != "" {
		auth += " (" + p.Auth.Env + ")"
	}
	if note != "" {
		source += " (" + note + ")"
	}
	lines := []string{"profile   " + p.Name, "protocol  " + p.Protocol, "auth      " + auth, "cred      " + source, "routes:"}
	for _, d := range p.Dialects() {
		r, _ := p.Route(d)
		lines = append(lines, fmt.Sprintf("  %-10s %s", d, r))
	}
	lines = append(lines, "models:")
	for _, s := range models.AliasNames {
		if id := p.Models[s]; id != "" {
			lines = append(lines, fmt.Sprintf("  %-10s %s", s, id))
		}
	}
	return lines
}

// providerMode is how p authenticates: oauth, m2m, pat, key, cli (a CLI's own login), or "-".
func providerMode(p *Provider) string {
	switch m := p.Auth.Method(); {
	case p.Preset == PresetDatabricks && m == AuthKey:
		return AuthOAuth
	case p.Auth.Login != "":
		return AuthCLI
	case m == AuthNone || m == "":
		return "-"
	default:
		return m
	}
}

// familyProfiles joins names, marking the session default with *.
func familyProfiles(reg *Registry, names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n
		if n == reg.Default {
			out[i] = "*" + n
		}
	}
	return strings.Join(out, ", ")
}

// sortProfiles orders a group's profiles by name; no profile label gets a hardcoded rank.
func sortProfiles(names []string) []string {
	slices.Sort(names)
	return names
}

func appendUnique(xs []string, x string) []string {
	if slices.Contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

// SpecsByAlias maps each model alias the selection resolves to its target spec.
func (s Selection) SpecsByAlias() map[string]string {
	byAlias := map[string]string{}
	for _, a := range models.AliasNames {
		if t, ok := s.Aliases[a]; ok {
			byAlias[a] = t.Spec()
		}
	}
	return byAlias
}

// SelectionLines renders the text view of `models`: the profile and proxy line, then each alias's resolution.
func SelectionLines(reg *Registry, sel Selection) []string {
	lines := []string{fmt.Sprintf("profile  %s   proxy:%v", sel.Provider.Name, sel.NeedsRouter(reg))}
	for _, a := range models.AliasNames {
		if t, ok := sel.Aliases[a]; ok {
			lines = append(lines, fmt.Sprintf("  %-7s %-40s → %s", a, sel.ClientModel(a), t.Spec()))
		}
	}
	return lines
}

// LiveModel is the JSON row for one catalog entry.
type LiveModel struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	UseCase      string `json:"use_case"`
	Capabilities string `json:"capabilities,omitempty"`
	Task         string `json:"task,omitempty"`
	Ready        string `json:"ready,omitempty"`
	Gateway      bool   `json:"gateway,omitempty"`
}

// Catalog is a provider's live model listing: its own models, the AI gateway's, and what to tell the user.
type Catalog struct {
	Infos, Gateway []ModelInfo
	Note           string // set when Infos came from the offline fallback list
	GatewayErr     error
}

// LoadCatalog fetches p's catalog (listOwn for a provider with its own SDK, else a plain /models GET), falling
// back to a known offline list where one exists, then the gateway's models.
func LoadCatalog(reg *Registry, p *Provider, listOwn func(context.Context) ([]ModelInfo, error)) (Catalog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var c Catalog
	var err error
	if listOwn != nil {
		c.Infos, err = listOwn(ctx)
	} else {
		c.Infos, err = ListModels(ctx, reg, p)
	}
	if err != nil {
		if c.Infos, c.Note, err = FallbackModels(p, err); err != nil {
			return Catalog{}, err
		}
	}
	c.Gateway, c.GatewayErr = GatewayModels(ctx, p)
	return c, nil
}

// Rows is every catalog entry, gateway included, as JSON rows.
func (c Catalog) Rows() []LiveModel {
	rows := make([]LiveModel, 0, len(c.Infos)+len(c.Gateway))
	for _, m := range append(slices.Clone(c.Infos), c.Gateway...) {
		rows = append(rows, LiveModel(m))
	}
	return rows
}

// TableLines is the text view: a Databricks serving-endpoints table plus the gateway's, or one merged model table.
func (c Catalog) TableLines() []string {
	if len(c.Infos) > 0 && c.Infos[0].Task != "" {
		lines := endpointTableLines(c.Infos)
		if len(c.Gateway) > 0 {
			lines = append(append(lines, "", "AI GATEWAY (anthropic)"), modelTableLines(c.Gateway)...)
		}
		return lines
	}
	return modelTableLines(append(slices.Clone(c.Infos), c.Gateway...))
}

// endpointTableLines renders a Databricks serving-endpoints listing as aligned name/task/ready columns.
func endpointTableLines(infos []ModelInfo) []string {
	nameW, taskW := len("NAME"), len("TASK")
	for _, m := range infos {
		nameW, taskW = max(nameW, len(m.ID)), max(taskW, len(m.Task))
	}
	lines := []string{fmt.Sprintf("%-*s  %-*s  %s", nameW, "NAME", taskW, "TASK", "READY")}
	for _, m := range infos {
		lines = append(lines, strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %s", nameW, m.ID, taskW, m.Task, m.Ready), " "))
	}
	return lines
}

// modelTableLines renders an aligned ID/Model/Use case/Capabilities table; widths come from the rows, and a
// gemini "models/" id prefix is dropped for display only (JSON keeps the raw id).
func modelTableLines(infos []ModelInfo) []string {
	idW, nameW, caseW, capW := len("ID"), len("Model"), len("Use case"), len("Capabilities")
	ids := make([]string, len(infos))
	for i, m := range infos {
		ids[i] = strings.TrimPrefix(m.ID, "models/")
		idW, nameW = max(idW, len(ids[i])), max(nameW, len(m.Name))
		caseW, capW = max(caseW, len(m.UseCase)), max(capW, len(m.Capabilities))
	}
	row := func(a, b, c, d string) string {
		return strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %-*s  %-*s", idW, a, nameW, b, caseW, c, capW, d), " ")
	}
	lines := []string{row("ID", "Model", "Use case", "Capabilities")}
	for i, m := range infos {
		lines = append(lines, row(ids[i], m.Name, m.UseCase, m.Capabilities))
	}
	return lines
}

// DryRunLines is the shared --dry-run body for switch and models: the record change plus notes, or that there is none.
func DryRunLines(before, after map[string]vault.AgentRecord, notes []string) []string {
	diff := append(recordDiff(before, after), notes...)
	if len(diff) == 0 {
		return []string{"no change to the agent records"}
	}
	lines := []string{"would write the agent records:"}
	for _, l := range diff {
		lines = append(lines, "  "+l)
	}
	return lines
}
