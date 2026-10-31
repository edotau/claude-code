// agentpin.go — the Profile row every agent page carries: the profile an agent runs on, or off; plus record writes.
package configure

import (
	"cmp"
	"reflect"
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

// agentPinRows is agent's one Profile row.
func agentPinRows(d *dialog, agent string) []billingRow {
	cur := agentpkg.GetAgentRecord(agent).Profile
	state := bracket(true, seedLabel(agent, cur), "")
	if why := agentpkg.AgentUnbillable(agent); cur != "" && why != "" {
		state = "[off, " + why + "]"
	}
	return []billingRow{{label: "Profile", desc: "profile " + agent + " runs on " + state,
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
	names := seedProfiles(d, agent)
	if agent == "opencode" {
		names = slices.DeleteFunc(names, func(n string) bool { return n == agentpkg.OpencodeOwnLogin })
		names = slices.Insert(names, 0, agentpkg.OpencodeOwnLogin)
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
		rows[i] = [2]string{seedLabel(agent, n), curMark(n == cur)}
	}
	idx, err := pick(d, displayLabel(agent)+" — profile", rows, max(slices.Index(names, cur), 0))
	if err != nil || idx < 0 || idx >= len(names) {
		return false, nil
	}
	return writeAgentField(d, agent, "PROFILE", names[idx])
}

// agentActiveOn turns an off agent on after a successful sign-in, by seeding its own login's profile.
func agentActiveOn(d *dialog, agent string) {
	if agent == "claude" || agentpkg.GetAgentRecord(agent).Profile != "" {
		return
	}
	if profile := signInProfile(d, agent); profile != "" {
		seedFromSignIn(d, agent, profile)
	}
}

// signInProfile is the profile a sign-in to agent means: its own login's profile, "" when none is registered.
func signInProfile(d *dialog, agent string) string {
	if agent == "opencode" {
		return agentpkg.OpencodeOwnLogin
	}
	if names := seedProfiles(d, agent); len(names) > 0 && d.reg != nil && d.reg.Providers[names[0]].Auth.Login == agent {
		return names[0]
	}
	return ""
}

// writeAgentField sets one record field (a vault.AgentRecord.WithPin name) and prints it; (false, nil) if unchanged.
func writeAgentField(d *dialog, agent, field, val string) (bool, error) {
	rec := agentpkg.GetAgentRecord(agent)
	next := rec.WithPin(field, val)
	if reflect.DeepEqual(rec, next) {
		return false, nil
	}
	if err := agentpkg.SetAgentRecord(agent, next); err != nil {
		return false, err
	}
	if field == "PROFILE" {
		if err := agentpkg.ClearLastProfilesFor(agent); err != nil {
			return false, err
		}
	}
	d.printf("  %s %s: %s\n", agent, strings.ToLower(field), cmp.Or(val, "unset"))
	d.changed = true
	return true, nil
}
