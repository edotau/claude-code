package agent

import (
	"fmt"
	"slices"
	"strings"
)

// RosterRow is one agent in `agents`: its binary, availability, capability levels and event kinds.
type RosterRow struct {
	Name      string            `json:"name"`
	Binary    string            `json:"binary"`
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"`
	Caps      map[string]string `json:"caps"`
	Events    []string          `json:"events"`
	Levels    Caps              `json:"-"`
	shown     []string
}

// rosterShown are the capabilities the text roster lists beside each agent.
var rosterShown = []string{"model", "profile", "effort", "workdir", "session", "tools", "system-prompt",
	"append-system-prompt", "env", "output-schema"}

// RosterRows describes every registered agent, probing each one's availability.
func RosterRows() []RosterRow {
	var rows []RosterRow
	for _, r := range Engines() {
		row := RosterRow{Available: true}
		if err := r.Available(); err != nil {
			row.Available, row.Reason = false, err.Error()
		}
		s, _ := LookupSchema(r.Name())
		row.Caps = map[string]string{}
		for _, f := range CapFields(s.CLI.Caps) {
			row.Caps[f.Name] = f.L.String()
			if f.L != None && slices.Contains(rosterShown, f.Name) {
				row.shown = append(row.shown, f.Name+":"+f.L.String())
			}
		}
		row.Caps["cost"], row.Caps["live"] = None.String(), None.String()
		if s.CLI.Cost {
			row.Caps["cost"] = Native.String()
		}
		if s.HasLive() {
			row.Caps["live"] = Native.String()
		}
		row.Caps["background"], row.Caps["remote"] = Native.String(), Native.String()
		row.Binary = s.CLI.Binary
		if !s.External() {
			row.Binary = s.Name
		}
		row.Name, row.Events, row.Levels = s.Name, s.CLI.Events, s.CLI.Caps
		rows = append(rows, row)
	}
	return rows
}

// Line is the text roster row: availability mark, name, the shown capabilities and the unavailable reason.
func (r RosterRow) Line() string {
	mark := "✓"
	if !r.Available {
		mark = "✗"
	}
	return fmt.Sprintf("%s %-9s %s %s", mark, r.Name, strings.Join(r.shown, ","), r.Reason)
}

// CapFields lists an agent's capability levels in RunOptions-flag order, named as the flags are.
func CapFields(c Caps) []struct {
	Name string
	L    Level
} {
	return []struct {
		Name string
		L    Level
	}{{"model", c.Model}, {"profile", c.Provider}, {"effort", c.Effort}, {"workdir", c.WorkDir}, {"session", c.SessionID},
		{"tools", c.Tools}, {"no-tools", c.NoTools}, {"autonomous", c.Autonomous}, {"fleet", c.Fleet}, {"enable-memory", c.Memory},
		{"no-persist", c.NoPersist}, {"safe-mode", c.SafeMode}, {"debug-file", c.DebugFile}, {"direct", c.Direct},
		{"system-prompt", c.SystemPrompt}, {"append-system-prompt", c.AppendSystemPrompt}, {"env", c.Env},
		{"output-schema", c.OutputSchema}, {"persona", c.Persona}}
}
