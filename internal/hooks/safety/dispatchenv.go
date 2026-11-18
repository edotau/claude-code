package safety

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// cmdStart anchors a pattern at a command word; secretpipe.go builds on it.
const cmdStart = "(?:^|[;&|({`])\\s*(?:rtk\\s+)?(?:(?:exec|nohup|time|command|builtin|nice|stdbuf\\S*)\\s+(?:-\\S+\\s+)*)*"

const wipeName = "every variable (env -i)"

var (
	dispatchVarName = regexp.MustCompile(`^HARNESS_DISPATCH_[A-Z_]+$`)
	shellString     = regexp.MustCompile(`(?:^|[\s;&|(])(?:(?:ba|z|da|k)?sh\s+(?:-\w+\s+)*-\w*c|eval)\s+(?:'([^']*)'|"([^"]*)")`)
	launchers       = []string{"claude-code", "claude", "codex", "copilot", "gemini", "agy", "opencode"}
	declarers       = []string{"export", "declare", "typeset", "readonly", "local"}
	dispatchAlias   = regexp.MustCompile(`(?:^|[\s;&|(])([A-Za-z_]\w*)=["']?(HARNESS_DISPATCH\w*)`)
)

// dispatchTamper is the variable a leaf's command, or a shell string inside it, clears or overrides, else "".
func dispatchTamper(in *core.Input, _ string) string { // reads the raw command: the scan blanks a heredoc's owner line
	if !isLeaf(in) {
		return ""
	}
	queue := []string{in.ToolCommand()}
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

// tamperName is the dispatch variable one command text changes (wipeName for an env wipe), else "".
func tamperName(text string) string {
	if !strings.Contains(text, "HARNESS_DISPATCH_") && !strings.Contains(text, "env") {
		return ""
	}
	segs, seps := shell.SegmentsSep(text)
	body, aliases := shell.HeredocBodies(segs, seps), map[string]string{}
	for _, m := range dispatchAlias.FindAllStringSubmatch(text, -1) {
		aliases[m[1]] = m[2]
	}
	for i, seg := range segs {
		if body[i] {
			continue
		}
		toks := shell.Tokens(seg)
		if len(toks) > 0 {
			toks[0] = strings.TrimLeft(toks[0], "({`")
		}
		for _, start := range shell.CommandStarts(toks) {
			if name := startTamper(operands(start, aliases)); name != "" {
				return name
			}
		}
	}
	return ""
}

// startTamper checks one command start: its leading assignments, then an unset, declare or env command.
func startTamper(words []string) string {
	for _, w := range words {
		if !isAssignment(w) {
			break
		}
		if name := assignsDispatch(w); name != "" {
			return name
		}
	}
	cmd := skipAssign(words)
	for len(cmd) > 0 && (cmd[0] == "builtin" || cmd[0] == "command") {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return ""
	}
	switch name := filepath.Base(cmd[0]); {
	case name == "unset":
		return firstDispatchName(cmd[1:])
	case name == "env":
		if n := envTamper(cmd[1:]); n != "" {
			return n
		}
	case slices.Contains(declarers, name):
		return declareTamper(cmd[1:])
	}
	text, ok := shell.NestedCommand(cmd)
	if !ok {
		text, ok = shell.CString(cmd)
	}
	if ok {
		return tamperName(text)
	}
	return ""
}

// operands expands a {a,b} list and a $VAR that a name=HARNESS_DISPATCH* assignment on the line binds, in each word.
func operands(words []string, aliases map[string]string) []string {
	var out []string
	for _, w := range words {
		w = os.Expand(w, func(k string) string { return cmp.Or(aliases[k], "$"+k) })
		list, suffix, _ := strings.Cut(w, "}") // Tokens drops a word's closing brace
		open := strings.IndexByte(list, '{')
		if open < 0 || !strings.Contains(list, ",") {
			out = append(out, w)
			continue
		}
		for item := range strings.SplitSeq(list[open+1:], ",") {
			out = append(out, list[:open]+item+suffix)
		}
	}
	return out
}

func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	return ok && name != "" && !strings.ContainsAny(name, "-/ ")
}

// assignsDispatch is the dispatch variable a NAME=value word sets, else "".
func assignsDispatch(w string) string {
	if name, _, ok := strings.Cut(w, "="); ok && dispatchVarName.MatchString(name) {
		return name
	}
	return ""
}

func firstDispatchName(words []string) string {
	for _, w := range words {
		if dispatchVarName.MatchString(w) {
			return w
		}
	}
	return ""
}

// declareTamper: an assignment, `export -n NAME`, or `declare|typeset +x NAME` of a dispatch variable.
func declareTamper(args []string) string {
	drops := slices.Contains(args, "-n") || slices.Contains(args, "+x")
	for _, a := range args {
		if name := assignsDispatch(a); name != "" {
			return name
		}
		if drops && dispatchVarName.MatchString(a) {
			return a
		}
	}
	return ""
}

// envTamper: env assignments or -u of a dispatch variable, or a wipe (-i, -) before a launcher.
func envTamper(args []string) string {
	wipe := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-" || a == "--ignore-environment" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i"):
			wipe = true
		case a == "-u" || a == "--unset":
			if i++; i < len(args) && dispatchVarName.MatchString(args[i]) {
				return args[i]
			}
		case strings.HasPrefix(a, "--unset="), strings.HasPrefix(a, "-u") && !strings.HasPrefix(a, "--"):
			if v := strings.TrimPrefix(strings.TrimPrefix(a, "--unset="), "-u"); dispatchVarName.MatchString(v) {
				return v
			}
		case assignsDispatch(a) != "":
			return assignsDispatch(a)
		}
	}
	if wipe && runsLauncher(args, 0) {
		return wipeName
	}
	return ""
}

// runsLauncher reports env's operand command (or a shell string it runs) naming a launcher.
func runsLauncher(args []string, depth int) bool {
	rest, _ := shell.Unwrap(append([]string{"env"}, args...), "")
	if len(rest) > 0 && rest[0] == "-" {
		rest = rest[1:]
	}
	if len(rest) > 0 && slices.Contains(launchers, filepath.Base(rest[0])) {
		return true
	}
	text, ok := shell.NestedCommand(rest)
	if !ok || depth > 3 {
		return false
	}
	segs, _ := shell.SegmentsSep(text)
	for _, seg := range segs {
		for _, st := range shell.CommandStarts(shell.Tokens(seg)) {
			if w := skipAssign(st); len(w) > 0 && (slices.Contains(launchers, filepath.Base(w[0])) || runsLauncher(w, depth+1)) {
				return true
			}
		}
	}
	return false
}

func skipAssign(w []string) []string {
	for len(w) > 0 && isAssignment(w[0]) {
		w = w[1:]
	}
	return w
}
