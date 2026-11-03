package settings

import "strings"

// HookModules wraps the module names from Specs into help lines under 80 columns.
func HookModules() string {
	var lines []string
	line := "modules: "
	for _, sp := range Specs() {
		if len(line)+len(sp.Name)+1 > 79 {
			lines = append(lines, line)
			line = "         "
		}
		line += sp.Name + "|"
	}
	return strings.Join(append(lines, strings.TrimSuffix(line, "|")), "\n")
}
