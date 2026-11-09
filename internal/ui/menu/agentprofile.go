// agentpin.go — the Profile row every agent page carries: the profile an agent runs on, or off; plus record writes.
package menu

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/configure"

	"github.com/edotau/claude-code/internal/vault"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
)

// agentPinRows is agent's one Profile row.
func agentPinRows(d *dialog, agent string) []billingRow {
	cur := vault.GetAgentRecord(agent).Profile
	provider := "pinned profile"
	if d.reg != nil {
		if p := d.reg.Providers[cur]; p != nil && p.Provider != "" {
			provider = p.Provider
		}
	}
	state := Checked(cur != "", seedLabel(agent, cur)) // green when a profile is pinned
	if why := agentpkg.AgentUnbillable(agent); cur != "" && why != "" {
		state = "[off, " + why + "]"
	}
	return []billingRow{{label: "Profile", desc: provider + " " + state,
		run: func() bool {
			changed, err := pickAgentProfile(d, agent, cur)
			if err != nil {
				d.printf("%s: %v\n", agent, err)
			}
			return changed
		}}}
}

// agentProfileChoices is the picker's profile values: off ("") first, then the own login, then the rest.
func agentProfileChoices(d *dialog, agent string) []string {
	names := configure.SeedProfiles(d.reg, agent)
	if agent == "opencode" {
		names = slices.DeleteFunc(names, func(n string) bool { return n == opencode.OwnLogin })
		names = slices.Insert(names, 0, opencode.OwnLogin)
		for _, id := range providers.RouteOpencodeSubIDs {
			if !slices.Contains(names, id) {
				names = append(names, id)
			}
		}
	}
	return append([]string{""}, names...)
}

// pickAgentProfile lists off (automatic for claude) and the profiles agent can run on; a pick writes the record.
// A dismissed picker or an unchanged profile is (false, nil); a failed write is (false, err).
func pickAgentProfile(d *dialog, agent, cur string) (bool, error) {
	names := agentProfileChoices(d, agent)
	rows := make([][2]string, len(names))
	for i, n := range names {
		rows[i] = [2]string{seedLabel(agent, n), uidialog.Mark(n == cur)}
	}
	idx, err := d.shell().Pick(displayLabel(agent)+" — profile", rows, max(slices.Index(names, cur), 0))
	if err != nil || idx < 0 || idx >= len(names) {
		return false, nil
	}
	return writeAgentField(d, agent, "PROFILE", names[idx])
}

// agentActiveOn turns an off agent on after a successful sign-in, by seeding its own login's profile.
func agentActiveOn(d *dialog, agent string) {
	if agent == "claude" || vault.GetAgentRecord(agent).Profile != "" {
		return
	}
	if profile := signInProfile(d, agent); profile != "" {
		seedFromSignIn(d, agent, profile)
	}
}

// signInProfile is the profile a sign-in to agent means: its own login's profile, "" when none is registered.
func signInProfile(d *dialog, agent string) string {
	if agent == "opencode" {
		return opencode.OwnLogin
	}
	if names := configure.SeedProfiles(d.reg, agent); len(names) > 0 && d.reg != nil && d.reg.Providers[names[0]].Auth.Login == agent {
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

// seed.go — the "Seed agent profiles" dialog: choose, per agent, the profile it runs on (or off); first run opens it.

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

// seedOwnLogin reports whether profile is agent's own sign-in (opencode's Zen id has no registry row).
func seedOwnLogin(d *dialog, agent, profile string) bool {
	return profile == opencode.OwnLogin || d.reg.Providers[profile].Auth.Login == agent
}

// seedDefaults is each agent's current choice; an agent with no profile set gets its own login pre-selected.
func seedDefaults(d *dialog) map[string]string {
	recs := vault.AgentRecords(agentpkg.RosterNames())
	out := map[string]string{}
	for _, a := range seedAgents() {
		out[a] = recs[a].Profile
		if out[a] == "" && a != "claude" && d.reg != nil {
			if ps := configure.SeedProfiles(d.reg, a); len(ps) > 0 && seedOwnLogin(d, a, ps[0]) {
				out[a] = ps[0]
			}
		}
	}
	return out
}

// seedPick opens agent's picker; ok is false when dismissed.
func seedPick(d *dialog, agent, cur string) (profile string, ok bool) {
	names := append(configure.SeedProfiles(d.reg, agent), "")
	rows := make([][2]string, len(names))
	for i, n := range names {
		rows[i] = [2]string{seedLabel(agent, n), uidialog.Mark(n == cur)}
	}
	idx, err := d.shell().Pick(displayLabel(agent)+" — profile", rows, max(slices.Index(names, cur), 0))
	if err != nil || idx < 0 || idx >= len(names) {
		return "", false
	}
	return names[idx], true
}

// runSeed shows one row per agent and writes every choice, Profile only, when Save is picked; dismissing writes nothing.
func runSeed(d *dialog) bool {
	choice, agents, saved := seedDefaults(d), seedAgents(), false
	d.shell().Run("Seed agent profiles", func() []uidialog.Row {
		rows := make([]uidialog.Row, 0, len(agents)+1)
		for _, a := range agents {
			rows = append(rows, uidialog.Row{Name: displayLabel(a), Detail: seedLabel(a, choice[a]), Run: func() bool {
				if p, ok := seedPick(d, a, choice[a]); ok {
					choice[a] = p
				}
				return false
			}})
		}
		return append(rows, uidialog.Row{Name: "Save", Detail: "write these choices", Run: func() bool {
			saved = saveSeed(d, choice)
			return true
		}})
	})
	return saved
}

// saveSeed stores choice's profiles in one update, leaving every other record field as it was.
func saveSeed(d *dialog, choice map[string]string) bool {
	agents := make([]string, 0, len(choice))
	for a, p := range choice {
		if err := configure.ValidProfile([]string{a}, p); err != nil {
			d.printf("seed: %v\n", err)
			return false
		}
		agents = append(agents, a)
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
	d.changed = true // the profiles are persisted: configs must re-render even if the cache cleanup below fails
	if err := providers.ClearLastProfilesFor(agents...); err != nil {
		d.printf("seed: %v\n", err)
		return false
	}
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

// seedFromSignIn records profile as agent's Profile after a successful sign-in, changing no other field.
func seedFromSignIn(d *dialog, agent, profile string) {
	if _, err := configure.ApplyProfile([]string{agent}, profile); err != nil {
		d.printf("%s: %v\n", agent, err)
		return
	}
	d.changed = true
}
