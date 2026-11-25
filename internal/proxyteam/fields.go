package proxyteam

import "strings"

// Row is the team name then each other non-empty field as name=value, on one line.
func (t Team) Row() string {
	var parts []string
	for i, f := range t.Fields() {
		switch {
		case i == 0:
			parts = append(parts, f.Value)
		case f.Value != "" && f.Value != "false":
			parts = append(parts, f.Name+"="+f.Value)
		}
	}
	return strings.Join(parts, " ")
}
