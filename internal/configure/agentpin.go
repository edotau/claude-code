// agentpin.go — the Active and Route rows every agent page carries: switch an agent off/on and pin its profile.
package configure

import (
	"slices"

	"github.com/edotau/claude-code/internal/providers"
)

// agentPinRows is agent's Active row (not for claude, which is always on) and its Route row.
func agentPinRows(d *dialog, agent string) []billingRow {
	var rows []billingRow
	if key := providers.AgentActivePin(agent); key != "" {
		on := providers.AgentActive(agent)
		rows = append(rows, billingRow{label: "Active", desc: "route work to " + agent + " " + bracket(on, "on", "off"),
			run: func() bool {
				next := providers.AgentOff
				if !on {
					next = providers.AgentOn
				}
				return setAgentPins(d, agent, map[string]string{key: next})
			}})
	}
	key := providers.AgentProfilePin(agent)
	if key == "" {
		return rows
	}
	pin := providers.Pin(key)
	rows = append(rows, billingRow{label: "Route", desc: "pin the profile this agent runs on " + bracket(pin != "", pin, "default"),
		run: func() bool {
			changed, err := pickAgentRoute(d, agent, key, pin)
			if err != nil {
				d.printf("%s: %v\n", agent, err)
			}
			return changed
		}})
	return rows
}

// pickAgentRoute lists the profiles agent can run on (opencode also its sub ids), then Default to clear the pin.
// A dismissed picker or an unchanged pin is (false, nil); a failed pin write is (false, err).
func pickAgentRoute(d *dialog, agent, key, cur string) (bool, error) {
	var names []string
	if d.reg != nil {
		for n, p := range d.reg.Providers {
			if p != nil && providers.CanServe(agent, p) {
				names = append(names, n)
			}
		}
	}
	if agent == "opencode" {
		for _, id := range providers.RouteOpencodeSubIDs {
			if !slices.Contains(names, id) {
				names = append(names, id)
			}
		}
	}
	slices.Sort(names)
	rows := make([][2]string, 0, len(names)+1)
	for _, n := range names {
		rows = append(rows, [2]string{n, curMark(n == cur)})
	}
	rows = append(rows, [2]string{"Default", "clear the pin" + curMark(cur == "")})
	idx, err := pick(d, displayLabel(agent)+" — route", rows)
	if err != nil || idx < 0 {
		return false, nil
	}
	val := ""
	if idx < len(names) {
		val = names[idx]
	}
	return writeAgentPins(d, map[string]string{key: val})
}

// agentActiveOn switches agent back on after a successful sign-in, when its Active pin is off.
func agentActiveOn(d *dialog, agent string) {
	if key := providers.AgentActivePin(agent); key != "" && !providers.AgentActive(agent) {
		setAgentPins(d, agent, map[string]string{key: providers.AgentOn})
	}
}

// setAgentPins writes kv and prints each changed pin; false when nothing changed or the write failed.
func setAgentPins(d *dialog, agent string, kv map[string]string) bool {
	changed, err := writeAgentPins(d, kv)
	if err != nil {
		d.printf("%s: %v\n", agent, err)
	}
	return changed
}

// writeAgentPins writes kv and prints each changed pin; (false, nil) when nothing changed, (false, err) on a failed write.
func writeAgentPins(d *dialog, kv map[string]string) (bool, error) {
	diff, err := providers.PinDiff(kv)
	if err != nil {
		return false, err
	}
	if len(diff) == 0 {
		return false, nil
	}
	if err := providers.SetPins(kv); err != nil {
		return false, err
	}
	for _, l := range diff {
		d.printf("  %s\n", l)
	}
	d.changed = true
	return true, nil
}
