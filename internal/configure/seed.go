// seed.go — the "Seed agent profiles" dialog: choose, per agent, the profile it runs on (or off); first run opens it.
package configure

import (
	"fmt"
	"github.com/edotau/claude-code/internal/vault"
	"slices"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
)

const (
	seedOff  = "off"
	seedAuto = "automatic"
)

// seedAgents is claude then every external agent, the dialog's rows in order.
func seedAgents() []string { return append([]string{"claude"}, agentpkg.Names()...) }

// seedProfiles lists the profiles agent can serve, its own login first; empty when the registry is unloaded.
func seedProfiles(d *dialog, agent string) []string {
	var own, rest []string
	if d.reg != nil {
		for n, p := range d.reg.Providers {
			switch {
			case p == nil || !agentpkg.CanServe(agent, p):
			case agent != "claude" && p.Auth.Login == agent:
				own = append(own, n)
			default:
				rest = append(rest, n)
			}
		}
	}
	slices.Sort(own)
	slices.Sort(rest)
	if agent == "opencode" {
		own = append([]string{agentpkg.OpencodeOwnLogin}, own...)
	}
	return append(own, rest...)
}

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
	return profile == agentpkg.OpencodeOwnLogin || d.reg.Providers[profile].Auth.Login == agent
}

// seedDefaults is each agent's current choice; an agent with no profile set gets its own login pre-selected.
func seedDefaults(d *dialog) map[string]string {
	recs := vault.AgentRecords(agentpkg.RosterNames())
	out := map[string]string{}
	for _, a := range seedAgents() {
		out[a] = recs[a].Profile
		if out[a] == "" && a != "claude" && d.reg != nil {
			if ps := seedProfiles(d, a); len(ps) > 0 && seedOwnLogin(d, a, ps[0]) {
				out[a] = ps[0]
			}
		}
	}
	return out
}

// seedPick opens agent's picker; ok is false when dismissed.
func seedPick(d *dialog, agent, cur string) (profile string, ok bool) {
	names := append(seedProfiles(d, agent), "")
	rows := make([][2]string, len(names))
	for i, n := range names {
		rows[i] = [2]string{seedLabel(agent, n), curMark(n == cur)}
	}
	idx, err := pick(d, displayLabel(agent)+" — profile", rows, max(slices.Index(names, cur), 0))
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
	for a, p := range choice {
		if _, err := ApplyProfile([]string{a}, p); err != nil {
			d.printf("seed: %v\n", err)
			return false
		}
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
	return homeRow{"seed agents", fmt.Sprintf("%d set", n), "profile each agent runs on", func(d *dialog) { runSeed(d) }}
}

// seedFromSignIn records profile as agent's Profile after a successful sign-in, changing no other field.
func seedFromSignIn(d *dialog, agent, profile string) {
	if _, err := ApplyProfile([]string{agent}, profile); err != nil {
		d.printf("%s: %v\n", agent, err)
		return
	}
	d.changed = true
}
