// agentprofile.go — the Profile row every agent page carries: the profile an agent runs on, or off; plus record writes.
package menu

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"

	"github.com/edotau/claude-code/internal/vault"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// agentPinRows is agent's Profile row, a record Model row when no catalog row exists, and the Fallback row.
func agentPinRows(d *dialog, agent string) []billingRow {
	cur := vault.GetAgentRecord(agent).Profile
	provider := "pinned profile"
	if d.reg != nil {
		if p := d.reg.Providers[cur]; p != nil && p.Provider != "" {
			provider = p.Provider
		}
	}
	state := checked(cur != "", seedLabel(agent, cur))
	if why := agentpkg.AgentUnbillable(agent); cur != "" && why != "" {
		state = "[off, " + why + "]"
	}
	rows := []billingRow{{label: "Pin / default", desc: "bare launch: " + provider + " " + state,
		run: func() bool {
			changed, err := pickAgentProfile(d, agent, cur)
			if err != nil {
				d.printf("%s: %v\n", agent, err)
			}
			return changed
		}}}
	if agent != "claude" && agent != "copilot" && modelRow(d, agent) == nil {
		rows = append(rows, recordModelRow(d, agent))
	}
	return append(rows, fallbackRow(d, agent))
}

// recordModelRow is the Model row for an agent with no catalog picker: it types the record's model, empty clears it.
func recordModelRow(d *dialog, agent string) billingRow {
	return billingRow{label: "Model", desc: textutil.Truncate(cmp.Or(vault.GetAgentRecord(agent).Model, "default"), 40),
		run: func() bool {
			val := strings.TrimSpace(d.shell().Line("Model for " + agent + " (empty clears): "))
			changed, err := writeAgentField(d, agent, "MODEL", val)
			if err != nil {
				d.printf("%s: %v\n", agent, err)
			}
			return changed
		}}
}

// fallbackRow is the Fallback row: the profile agent falls back to when its own is unavailable, none when empty.
func fallbackRow(d *dialog, agent string) billingRow {
	cur := vault.GetAgentRecord(agent).Fallback
	desc := "profile fails: " + checked(cur != "", cmp.Or(cur, "none"))
	if agent == "copilot" && cur == "" {
		if fb := copilotFallback(copilot.RouteChain(context.Background(), d.reg)); fb != "" {
			desc = fb
		}
	}
	return billingRow{label: "Fallback", desc: desc,
		run: func() bool {
			names := agentProfileChoices(d, agent)
			rows := make([][2]string, len(names))
			for i, n := range names {
				rows[i] = [2]string{cmp.Or(n, "none"), mark(n == cur)}
			}
			idx, err := d.shell().Pick(displayLabel(agent)+" — fallback profile", rows, max(slices.Index(names, cur), 0))
			if err != nil || idx < 0 || idx >= len(names) {
				return false
			}
			changed, err := writeAgentField(d, agent, "FALLBACK", names[idx])
			if err != nil {
				d.printf("%s: %v\n", agent, err)
			}
			return changed
		}}
}

// agentProfileChoices is the picker's profile values: off ("") first, then the own login, then the rest.
func agentProfileChoices(d *dialog, agent string) []string {
	names := configure.SeedProfiles(d.reg, agent)
	if agent == "opencode" {
		for _, id := range providers.RouteOpencodeSubIDs {
			if !slices.Contains(names, id) {
				names = append(names, id)
			}
		}
	}
	return append([]string{""}, names...)
}

// pickAgentProfile lists off (automatic for claude) and the profiles agent can run on; a pick writes the record.
func pickAgentProfile(d *dialog, agent, cur string) (bool, error) {
	name, ok := pickProfileName(d, agent, agentProfileChoices(d, agent), cur)
	if !ok {
		return false, nil
	}
	return writeAgentField(d, agent, "PROFILE", name)
}

// pickProfileName opens agent's profile picker over names with cur marked; ok is false when dismissed.
func pickProfileName(d *dialog, agent string, names []string, cur string) (name string, ok bool) {
	rows := make([][2]string, len(names))
	for i, n := range names {
		rows[i] = [2]string{seedLabel(agent, n), mark(n == cur)}
	}
	idx, err := d.shell().Pick(displayLabel(agent)+" — profile", rows, max(slices.Index(names, cur), 0))
	if err != nil || idx < 0 || idx >= len(names) {
		return "", false
	}
	return names[idx], true
}

// agentActiveOn turns an off agent on after a successful sign-in, by seeding its own login's profile.
func agentActiveOn(d *dialog, agent string) {
	if agent == "claude" || vault.GetAgentRecord(agent).Profile != "" {
		return
	}
	profile := signInProfile(d, agent)
	if profile == "" {
		return
	}
	if _, err := configure.ApplyProfile([]string{agent}, profile); err != nil {
		d.printf("%s: %v\n", agent, err)
		return
	}
	d.changed = true
}

// signInProfile is the profile a sign-in to agent means: its own login's profile, "" when none is registered.
func signInProfile(d *dialog, agent string) string {
	if agent == "claude" && d.reg != nil {
		return providers.ProfileName(d.reg.ProfileBy(anthropicProvider, providers.AuthCLI))
	}
	if agent == "opencode" {
		return opencode.OwnLogin
	}
	names := configure.SeedProfiles(d.reg, agent)
	if len(names) == 0 || d.reg == nil {
		return ""
	}
	if p := d.reg.Providers[names[0]]; p != nil && p.Auth.Login == agent {
		return names[0]
	}
	return ""
}

// writeAgentField sets one record field (a vault.AgentRecord.WithPin name) and prints it; (false, nil) if unchanged.
func writeAgentField(d *dialog, agent, field, val string) (bool, error) {
	if changed, err := configure.SetAgentPin(agent, field, val); !changed || err != nil {
		return false, err
	}
	d.printf("  %s %s: %s\n", agent, strings.ToLower(field), cmp.Or(val, "unset"))
	d.changed = true
	return true, nil
}

// Seed section — the "Seed agent profiles" dialog.

const (
	seedOff  = "off"
	seedAuto = "automatic"
)

// seedAgents is claude then every external agent, the dialog's rows in order.
func seedAgents() []string { return append([]string{"claude"}, agentpkg.Names()...) }

// seedLabel is the shown name of a stored profile: "" reads as automatic for claude and off for the rest.
func seedLabel(agent, profile string) string {
	switch {
	case profile != "":
		return profile
	case agent == "claude":
		return seedAuto
	}
	return seedOff
}

// seedDefaults is each agent's choice; only a first run (no profile set) pre-selects each agent's own login.
func seedDefaults(d *dialog) map[string]string {
	recs := vault.AgentRecords(agentpkg.RosterNames())
	firstRun := !slices.ContainsFunc(slices.Collect(maps.Values(recs)), func(r vault.AgentRecord) bool { return r.Profile != "" })
	out := map[string]string{}
	for _, a := range seedAgents() {
		out[a] = recs[a].Profile
		if firstRun && out[a] == "" && a != "claude" && d.reg != nil {
			out[a] = signInProfile(d, a)
		}
	}
	return out
}

// runSeed shows one row per agent and writes every choice.
func runSeed(d *dialog) {
	choice, agents := seedDefaults(d), seedAgents()
	d.shell().Run("Seed agent profiles", func() []shellRow {
		rows := make([]shellRow, 0, len(agents)+1)
		for _, a := range agents {
			rows = append(rows, shellRow{Name: displayLabel(a), Detail: seedLabel(a, choice[a]), Run: func() bool {
				if p, ok := pickProfileName(d, a, agentProfileChoices(d, a), choice[a]); ok {
					choice[a] = p
				}
				return false
			}})
		}
		return append(rows, shellRow{Name: "Save", Detail: "write these choices", Run: func() bool {
			saveSeed(d, choice)
			return true
		}})
	})
}

// saveSeed stores choice's profiles in one update, leaving every other record field as it was.
func saveSeed(d *dialog, choice map[string]string) bool {
	for a, p := range choice {
		if err := configure.ValidProfile([]string{a}, p); err != nil {
			d.printf("seed: %v\n", err)
			return false
		}
	}
	err := vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		for a, p := range choice {
			r := m[a]
			r.Profile = p
			m[a] = r
		}
	})
	if err != nil {
		d.printf("seed: %v\n", err)
		return false
	}
	d.changed = true
	d.println("agent profiles saved")
	return true
}

// seedOnFirstRun opens the dialog when no agent has a profile yet; off a terminal it only points at the row.
func seedOnFirstRun(d *dialog, section string) {
	if section != "" || d.o.Check || d.o.DryRun || d.o.JSON || d.o.Agent != "" {
		return
	}
	for _, r := range vault.AgentRecords(agentpkg.RosterNames()) {
		if r.Profile != "" {
			return
		}
	}
	if d.o.Interactive && isStdinTTY() {
		runSeed(d)
		return
	}
	d.println("no agent profiles set: run `claude-code configure` → seed agents")
}

func seedRow() homeRow {
	n := 0
	for _, r := range vault.AgentRecords(agentpkg.RosterNames()) {
		if r.Profile != "" {
			n++
		}
	}
	return homeRow{Name: "seed agents", State: fmt.Sprintf("%d set", n), Check: "profile each agent runs on", Run: func(d *dialog) { runSeed(d) }}
}

// databricksSignInRows pins an existing signed-in workspace through the shared profile writer.
func databricksSignInRows(d *dialog, agent string) []billingRow {
	if d.reg == nil {
		return nil
	}
	var rows []billingRow
	for _, name := range providers.DatabricksProfiles(d.reg) {
		p := d.reg.Providers[name]
		if !providers.CredentialPresent(p) {
			continue
		}
		rows = append(rows, billingRow{label: "Databricks " + name, desc: "use workspace " + mark(vault.GetAgentRecord(agent).Profile == name),
			run: func() bool {
				changed, err := writeAgentField(d, agent, "PROFILE", name)
				if err != nil {
					d.printf("%s: %v\n", agent, err)
				}
				return changed
			}})
	}
	return rows
}
