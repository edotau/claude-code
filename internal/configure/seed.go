// seed.go — the "Seed agent profiles" dialog: choose, per agent, the profile it runs on (or off); first run opens it.
package configure

import (
	"fmt"
	"slices"

	agentpkg "github.com/edotau/claude-code/internal/agent"
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
	recs := agentpkg.AgentRecords()
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
	choice, agents, cur := seedDefaults(d), seedAgents(), 0
	for {
		rows := make([][2]string, 0, len(agents)+1)
		for _, a := range agents {
			rows = append(rows, [2]string{displayLabel(a), seedLabel(a, choice[a])})
		}
		rows = append(rows, [2]string{"Save", "write these choices"})
		idx, err := pick(d, "Seed agent profiles", rows, cur)
		if err != nil || idx < 0 {
			return false
		}
		if idx == len(agents) {
			return saveSeed(d, choice)
		}
		cur = idx
		if p, ok := seedPick(d, agents[idx], choice[agents[idx]]); ok {
			choice[agents[idx]] = p
		}
	}
}

// saveSeed stores choice's profiles in one update, leaving every other record field as it was.
func saveSeed(d *dialog, choice map[string]string) bool {
	err := agentpkg.UpdateAgentRecords(func(m map[string]agentpkg.AgentRecord) {
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
	d.println("agent profiles saved")
	return true
}

// seedOnFirstRun opens the dialog when no agent has a profile yet; off a terminal it only points at the row.
func seedOnFirstRun(d *dialog, section string) {
	if section != "" || d.o.Check || d.o.DryRun || d.o.JSON || d.o.Agent != "" {
		return
	}
	for _, r := range agentpkg.AgentRecords() {
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
	for _, r := range agentpkg.AgentRecords() {
		if r.Profile != "" {
			n++
		}
	}
	return homeRow{"seed agents", fmt.Sprintf("%d set", n), "profile each agent runs on", func(d *dialog) { runSeed(d) }}
}

// seedFromSignIn records profile as agent's Profile after a successful sign-in, changing no other field.
func seedFromSignIn(d *dialog, agent, profile string) {
	err := agentpkg.UpdateAgentRecords(func(m map[string]agentpkg.AgentRecord) {
		r := m[agent]
		r.Profile = profile
		m[agent] = r
	})
	if err != nil {
		d.printf("%s: %v\n", agent, err)
		return
	}
	d.changed = true
}
