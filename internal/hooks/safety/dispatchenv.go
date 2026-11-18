package safety

import (
	"cmp"
	"regexp"

	"github.com/edotau/claude-code/internal/hooks/core"
)

const (
	dispatchVar = `HARNESS_DISPATCH_[A-Z_]+`
	cmdStart    = "(?:^|[;&|({`])\\s*(?:rtk\\s+)?(?:(?:exec|nohup|time|command|nice|stdbuf\\S*)\\s+(?:-\\S+\\s+)*)*"
	assignWord  = `\w+=(?:'[^']*'|"[^"]*"|\S*)`
	launcher    = `(?:claude-code|claude|codex|copilot|gemini|agy|opencode)`
	envWord     = `(?:\S*/)?env\s`
)

// dispatchTampers assign, export, unset or `env -u` a dispatch variable, or `env -i` before a launcher.
var dispatchTampers = []*core.Gated{
	core.Gate(cmdStart+`(?:(?:export|declare|typeset|readonly|local)(?:\s+-\S+)*\s+|env(?:\s+(?:-u\s*\S+|--unset[= ]\S+|-\S+|`+
		assignWord+`))*\s+)?(?:`+assignWord+`\s+)*`+dispatchVar+`=`, "HARNESS_DISPATCH_"),
	core.Gate(cmdStart+`unset\s[^;&|\n]*`+dispatchVar, "HARNESS_DISPATCH_"),
	core.Gate(cmdStart+envWord+`[^;&|\n]*(?:-u\s*|--unset[= ])`+dispatchVar, "HARNESS_DISPATCH_"),
	core.Gate(cmdStart+envWord+`[^;&|\n]*(?:-[a-zA-Z]*i|--ignore-environment)\s(?:[^;&|\n]*[\s/])?`+launcher+`(?:\s|$)`, "env"),
}

var (
	dispatchVarName = regexp.MustCompile(dispatchVar)
	shellString     = regexp.MustCompile(`(?:^|[\s;&|(])(?:(?:ba|z|da|k)?sh\s+(?:-\w+\s+)*-\w*c|eval)\s+(?:'([^']*)'|"([^"]*)")`)
)

// dispatchTamper is the variable a leaf's command, or a shell string inside it, clears or overrides, else "".
func dispatchTamper(in *core.Input, scan string) string {
	if !isLeaf(in) {
		return ""
	}
	queue := []string{scan}
	for i := 0; i < len(queue) && i < 8; i++ {
		if name := tamperName(queue[i]); name != "" {
			return name
		}
		for _, m := range shellString.FindAllStringSubmatch(queue[i], -1) {
			queue = append(queue, cmp.Or(m[1], m[2]))
		}
	}
	return ""
}

// tamperName is the dispatch variable one command text changes ("every variable (env -i)" for a wipe), else "".
func tamperName(text string) string {
	for i, re := range dispatchTampers {
		if !re.MatchString(text) {
			continue
		}
		if i == len(dispatchTampers)-1 {
			return "every variable (env -i)"
		}
		return dispatchVarName.FindString(re.Regexp().FindString(text))
	}
	return ""
}
