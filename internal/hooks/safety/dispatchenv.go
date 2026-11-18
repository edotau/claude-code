package safety

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// cmdStart anchors a pattern at a command word; secretpipe.go builds on it.
const cmdStart = "(?:^|[;&|({`])\\s*(?:rtk\\s+)?(?:(?:exec|nohup|time|command|builtin|nice|stdbuf\\S*)\\s+(?:-\\S+\\s+)*)*"

const wipeName = "every variable (env -i)"

var (
	dispatchVarName = regexp.MustCompile(`^HARNESS_DISPATCH_[A-Z_]+$`)
	dispatchName    = regexp.MustCompile(`HARNESS_DISPATCH_\w*`)
	launchers       = []string{"claude-code", "claude", "codex", "copilot", "gemini", "agy", "opencode"}
	declarers       = []string{"export", "declare", "typeset", "readonly", "local"}
	dispatchAlias   = regexp.MustCompile(`(?:^|[\s;&|(])([A-Za-z_]\w*)=["']?(HARNESS_DISPATCH\w*)`)
)

// tamperPhrase is an unset, declare, `env -u`, assignment or alias of a dispatch variable, in unquoted text.
var tamperPhrase = regexp.MustCompile(`(?:^|[^\w./-])(?:(?:unset|export|declare|typeset|readonly|local)\s(?:[^;&|\n]*?[\s=,{])?` +
	`|env\s(?:[^;&|\n]*?\s)?(?:-u\s*|--unset[=\s]?)|[A-Za-z_]\w*=)HARNESS_DISPATCH_\w*|(?:^|[^\w$.{/-])HARNESS_DISPATCH_\w*=`)

// unquoted drops shell quoting and backslashes, so `HARNESS\_DISPATCH_"ID"` reads as the name it is.
var unquoted = strings.NewReplacer(`\n`, " ", `\t`, " ", `\`, "", `'`, "", `"`, "")

// dispatchTamper is the dispatch variable a leaf's raw command clears or overrides, else "".
func dispatchTamper(in *core.Input) string {
	if !isLeaf(in) {
		return ""
	}
	cmd := in.ToolCommand()
	if name := tamperName(cmd); name != "" {
		return name
	}
	// An evaluator may run any text on the line however it gets there, so a tamper phrase anywhere counts.
	if m := tamperPhrase.FindString(unquoted.Replace(cmd)); m != "" && shell.Evaluates(cmd) {
		return dispatchName.FindString(m)
	}
	return ""
}

// tamperName is the dispatch variable one command text changes (wipeName for an env wipe), else "".
func tamperName(text string) string {
	if !strings.Contains(unquoted.Replace(text), "HARNESS_DISPATCH_") && !strings.Contains(text, "env") {
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

// readKubeconfig is the seam for reading a kubeconfig as text.
var readKubeconfig = os.ReadFile

// mainOnly is a main-session-only command: asked in the main session, refused in a leaf.
type mainOnly struct{ what, hint string }

// mainOnlyVerdict asks (main) or blocks (leaf) a vault key read, a production exec/delete or a leaf's reserved make.
func mainOnlyVerdict(in *core.Input) (core.Decision, string, string) {
	cmd := in.ToolCommand()
	if !slices.ContainsFunc([]string{"vault", "kubectl", "oc", "make"}, func(w string) bool { return strings.Contains(cmd, w) }) {
		return core.Allow, "", "" // the walk runs on every Bash call; most name none of these
	}
	hit := mainOnlyHit(in, cmd)
	if hit == nil {
		return core.Allow, "", ""
	}
	d := core.Ask
	if isLeaf(in) {
		d = core.Block
	}
	return d, core.SafetyMessage(in, d, hit.what, "", hit.hint), "main session only: " + hit.what
}

// mainOnlyHit is the first main-session-only command in cmd, else nil.
func mainOnlyHit(in *core.Input, cmd string) *mainOnly {
	var hit *mainOnly
	shell.EachCommand(cmd, in.CWD, func(dir string, w []string) bool {
		switch name := filepath.Base(w[0]); {
		case name == "claude-code" && vaultKey(w):
			hit = &mainOnly{"prints the vault master key", "Only the main session reads it, after the owner approves."}
		case (name == "kubectl" || name == "oc") && prodChange(w, cmd):
			hit = &mainOnly{"runs exec/delete against a production cluster",
				"The main session runs production changes after the owner approves."}
		case name == "make" && isLeaf(in) && reservedMake(w, dir):
			hit = &mainOnly{"runs make audit, install or fmt in the harness tree", "Run `make lint` and fix the files " +
				"it names that are yours; the main session runs audit, install and fmt from a clean tree."}
		}
		return hit != nil
	})
	return hit
}

// vaultKey reports `claude-code vault get` given --key (or -key, --key=true).
func vaultKey(w []string) bool {
	return len(w) > 2 && w[1] == "vault" && w[2] == "get" && slices.ContainsFunc(w[3:], func(a string) bool {
		k, v, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
		return strings.HasPrefix(a, "-") && k == "key" && (!has || v != "false" && v != "0")
	})
}

// kubeValueFlags take the next word as their value; kubeProdFlags' values name the cluster or namespace.
var (
	kubeValueFlags = []string{"-n", "--namespace", "--context", "--cluster", "--kubeconfig", "-s", "--server", "--user",
		"--token", "--as", "-c", "--container", "-l", "--selector", "-f", "--filename", "-o", "--output"}
	kubeProdFlags = []string{"-n", "--namespace", "--context", "--cluster", "--kubeconfig"}
	envWord       = regexp.MustCompile(`(?:^|\s)ENV=(\S+)`)
)

// prodChange reports kubectl/oc exec, delete or oc rsh whose flags, an ENV= word or the kubeconfig name production.
func prodChange(w []string, cmd string) bool {
	verb, flags := kubeArgs(w)
	if verb != "exec" && verb != "delete" && (verb != "rsh" || filepath.Base(w[0]) != "oc") {
		return false
	}
	var values []string
	for _, f := range kubeProdFlags {
		values = append(values, flags[f]...)
	}
	for _, m := range envWord.FindAllStringSubmatch(cmd, -1) {
		values = append(values, m[1])
	}
	if slices.ContainsFunc(values, prodName) {
		return true
	}
	if len(flags["--context"])+len(flags["--cluster"]) > 0 { // an explicit cluster that is not production
		return false
	}
	current := currentContext(strings.Join(flags["--kubeconfig"], ""))
	return current == "" || prodName(current) // an unknown context is treated as production
}

// kubeArgs is a kubectl/oc command's verb (its first operand) and each kubeValueFlags flag's values.
func kubeArgs(w []string) (verb string, flags map[string][]string) {
	flags = map[string][]string{}
	for i := 1; i < len(w) && w[i] != "--"; i++ {
		flag, val, has := strings.Cut(w[i], "=")
		switch {
		case !strings.HasPrefix(flag, "-"):
			verb = cmp.Or(verb, w[i])
		case !slices.Contains(kubeValueFlags, flag):
		case !has && i+1 < len(w):
			i, val = i+1, w[i+1]
			fallthrough
		default:
			flags[flag] = append(flags[flag], val)
		}
	}
	return verb, flags
}

// prodName reports prod, prd or production as one segment of v, split at - _ . / :.
func prodName(v string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return strings.ContainsRune("-_./:", r) }),
		func(s string) bool { return s == "prod" || s == "prd" || s == "production" })
}

// currentContext is the kubeconfig's current-context: path, else $KUBECONFIG's first entry, else ~/.kube/config.
func currentContext(path string) string {
	if first, _, _ := strings.Cut(os.Getenv("KUBECONFIG"), string(os.PathListSeparator)); path == "" {
		path = cmp.Or(first, filepath.Join("~", ".kube", "config"))
	}
	text, err := readKubeconfig(paths.ExpandHome(path))
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(text)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "current-context:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// reservedMakeTargets rewrite or ship the whole tree; `make lint` is a check a leaf may run.
var reservedMakeTargets = []string{"audit", "install", "fmt"}

// reservedMake reports a make of a reservedMakeTargets target whose directory (cwd or -C) is in the harness tree.
func reservedMake(w []string, dir string) bool {
	reserved := false
	for i := 1; i < len(w); i++ {
		a := w[i]
		switch {
		case a == "-C" || a == "--directory" || strings.HasPrefix(a, "--directory=") || len(a) > 2 && a[:2] == "-C":
			v := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(a, "--directory="), "--directory"), "-C")
			if v == "" && i+1 < len(w) {
				i, v = i+1, w[i+1]
			}
			if v = paths.ExpandHome(v); !filepath.IsAbs(v) {
				v = filepath.Join(dir, v)
			}
			dir = v
		case a == "-f" || a == "--file" || a == "-I" || a == "-o" || a == "-W":
			i++
		case !strings.HasPrefix(a, "-") && !strings.Contains(a, "="):
			reserved = reserved || slices.Contains(reservedMakeTargets, a)
		}
	}
	return reserved && (shell.UnknownDir(dir) || paths.InsideDir(dir, paths.ConfigDir()))
}
