package safety

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

const wipeName = "every variable (env -i)"

var (
	dispatchVarName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^HARNESS_DISPATCH_[A-Z_]+$`) })
	dispatchName    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`HARNESS_DISPATCH_\w*`) })
	dispatchAlias   = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?:^|[\s;&|(])([A-Za-z_]\w*)=["']?(HARNESS_DISPATCH\w*)`)
	})
)

// tamperPhrase is an unset, declare or `env -u` of a dispatch variable, or an assignment to one, in unquoted text.
var tamperPhrase = sync.OnceValue(func() *regexp.Regexp {
	unsetters := strings.Join(append([]string{"unset"}, declarers()...), "|")
	return regexp.MustCompile(`(?:^|[^\w./-])(?:(?:` + unsetters + `)\s(?:[^;&|\n]*?[\s=,{])?` +
		`|env\s(?:[^;&|\n]*?\s)?(?:-u\s*|--unset[=\s]?))HARNESS_DISPATCH_\w*|(?:^|[^\w$.{/-])HARNESS_DISPATCH_\w*=`)
})

var (
	launchers  = sync.OnceValue(func() []string { return shell.GuardWords("agent_launchers") })
	declarers  = sync.OnceValue(func() []string { return shell.GuardWords("shell_declarers") })
	kubeCLIs   = sync.OnceValue(func() map[string]bool { return shell.GuardSet("kube_clis") })
	shellNames = sync.OnceValue(func() map[string]bool { return shell.GuardSet("shell_names") })
)

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
	named := strings.Contains(unquoted.Replace(joinLiterals(cmd)), "HARNESS_DISPATCH_")
	if !named && !strings.Contains(cmd, "DISPATCH") && !runsInterpreter(cmd) {
		return ""
	}
	aliases := dispatchAliases(cmd)
	for _, text := range shell.EvaluatedText(cmd, in.CWD) { // text an evaluator runs, however it reaches it
		if name := interpTamper(text); name != "" {
			return name
		}
		text = os.Expand(text, func(k string) string { return cmp.Or(aliases[k], "$"+k) })
		if m := tamperPhrase().FindString(unquoted.Replace(text)); named && m != "" {
			return dispatchName().FindString(m)
		}
	}
	return ""
}

// literalJoin is a string-literal concatenation seam: "A"+"B", 'A'.'B' (perl), "A" "B" (python).
var literalJoin = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`["']\s*[+.]?\s*["']`) })

// joinLiterals folds literal concatenations, so "HARNESS_"+"DISPATCH_ID" reads as the name it builds.
func joinLiterals(text string) string { return literalJoin().ReplaceAllString(text, "") }

// envMutation is an interpreter's write, delete or merge into its environment (python, node, ruby, perl).
var envMutation = sync.OnceValue(func() *regexp.Regexp {
	const key = `(?:["'\x60]HARNESS_DISPATCH_\w*["'\x60]|\$?[A-Za-z_]\w*)` // the literal name, or a variable holding it
	return regexp.MustCompile(`os\.environ\s*\[\s*` + key + `\s*\]\s*=(?:[^=]|$)|\bdel\s+os\.environ\s*\[|` +
		`os\.(?:putenv|unsetenv)\s*\(\s*` + key + `|os\.environ\.(?:update|pop|setdefault)\s*\(|` +
		`process\.env\s*(?:\.HARNESS_DISPATCH_\w*|\[\s*` + key + `\s*\])\s*=(?:[^=]|$)|` +
		`\bdelete\s+process\.env\s*(?:\.HARNESS_DISPATCH_|\[)|Object\.assign\s*\(\s*process\.env\b|` +
		`\bENV\s*\[\s*` + key + `\s*\]\s*=(?:[^=~]|$)|\bENV\.(?:delete|store|update|merge!|replace)\s*\(|` +
		`\$ENV\s*\{\s*` + key + `\s*\}\s*=(?:[^=~]|$)|\bdelete\s+(?:local\s+)?\$ENV\s*\{`)
})

var envMarkers = []string{"ENV", "environ", "putenv", "unsetenv", "process.env"}

// interpTamper is the dispatch variable an interpreter text changes in its environment, else "".
func interpTamper(text string) string {
	if !slices.ContainsFunc(envMarkers, func(m string) bool { return strings.Contains(text, m) }) {
		return "" // every envMutation branch spells one of these outside any quote seam
	}
	text = joinLiterals(text)
	if !strings.Contains(text, "HARNESS_DISPATCH_") || !envMutation().MatchString(text) {
		return ""
	}
	return dispatchName().FindString(text)
}

// runsInterpreter reports a command start running an interpreter, whose script may build a dispatch name.
func runsInterpreter(cmd string) bool {
	for _, seg := range shell.Segments(cmd) {
		for _, st := range shell.CommandStarts(shell.Tokens(seg)) {
			if w := skipAssign(st); len(w) > 0 && shell.IsInterpreter(w[0]) {
				return true
			}
		}
	}
	return false
}

// dispatchAliases maps each NAME the text binds to a dispatch variable's name.
func dispatchAliases(text string) map[string]string {
	aliases := map[string]string{}
	for _, m := range dispatchAlias().FindAllStringSubmatch(text, -1) {
		aliases[m[1]] = m[2]
	}
	return aliases
}

// tamperName is the dispatch variable one command text changes (wipeName for an env wipe), else "".
func tamperName(text string) string {
	if !strings.Contains(unquoted.Replace(text), "HARNESS_DISPATCH_") && !strings.Contains(text, "env") {
		return ""
	}
	segs, seps := shell.SegmentsSep(text)
	body, aliases := shell.HeredocBodies(segs, seps), dispatchAliases(text)
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
	case slices.Contains(declarers(), name):
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
	if name, _, ok := strings.Cut(w, "="); ok && dispatchVarName().MatchString(name) {
		return name
	}
	return ""
}

func firstDispatchName(words []string) string {
	for _, w := range words {
		if dispatchVarName().MatchString(w) {
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
		if drops && dispatchVarName().MatchString(a) {
			return a
		}
	}
	return ""
}

// envTamper: env assignments or -u of a dispatch variable, or a wipe (-i, -) before a launcher or interpreter.
func envTamper(args []string) string {
	wipe := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-" || a == "--ignore-environment" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i"):
			wipe = true
		case a == "-u" || a == "--unset":
			if i++; i < len(args) && dispatchVarName().MatchString(args[i]) {
				return args[i]
			}
		case strings.HasPrefix(a, "--unset="), strings.HasPrefix(a, "-u") && !strings.HasPrefix(a, "--"):
			if v := strings.TrimPrefix(strings.TrimPrefix(a, "--unset="), "-u"); dispatchVarName().MatchString(v) {
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
	if len(rest) > 0 && launcher(rest[0]) {
		return true
	}
	text, ok := shell.NestedCommand(rest)
	if !ok || depth > 3 {
		return false
	}
	segs, _ := shell.SegmentsSep(text)
	for _, seg := range segs {
		for _, st := range shell.CommandStarts(shell.Tokens(seg)) {
			if w := skipAssign(st); len(w) > 0 && (launcher(w[0]) || runsLauncher(w, depth+1)) {
				return true
			}
		}
	}
	return false
}

// launcher reports an agent launcher or interpreter word; .hookd counts as it hands its verbs to claude-code.
func launcher(word string) bool {
	name := filepath.Base(word)
	return slices.Contains(launchers(), name) || shell.IsHarnessName(name) || shell.IsInterpreter(name)
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

// noAskRoute names where an ask still asks, for a main session on an agent with no ask decision.
const noAskRoute = " This agent has no ask: run it from a Claude session, where it asks, or from your own terminal."

// mainOnlyVerdict asks (main) or blocks (leaf) each main-session-only command mainOnlyHit finds.
func mainOnlyVerdict(in *core.Input) (core.Decision, string, string) {
	cmd := in.ToolCommand()
	plain := unquoted.Replace(cmd) // `ma''ke` is make
	if !slices.ContainsFunc([]string{"secret", "kubectl", "oc", "make"}, func(w string) bool { return strings.Contains(plain, w) }) {
		return core.Allow, "", "" // the walk runs on every Bash call; most name none of these
	}
	hit := mainOnlyHit(in, cmd)
	if hit == nil {
		return core.Allow, "", ""
	}
	d, hint := core.Ask, hit.hint
	if isLeaf(in) {
		d = core.Block
	} else if !core.SupportsAsk(in.Agent()) {
		hint += noAskRoute
	}
	return d, core.SafetyMessage(in, d, hit.what, "", hint), "main session only: " + hit.what
}

// mainOnlyHit is the first main-session-only command in cmd, else nil.
func mainOnlyHit(in *core.Input, cmd string) *mainOnly {
	var hit *mainOnly
	shell.EachCommand(cmd, in.CWD, func(dir string, w []string) bool {
		switch name := filepath.Base(w[0]); {
		case shell.IsHarnessName(name) && vaultKey(shell.HarnessArgs(w)):
			hit = &mainOnly{"prints the vault master key", "Only the main session reads it, after the owner approves."}
		case shell.IsHarnessName(name) && isLeaf(in) && shell.SecretFlag(shell.HarnessArgs(w)):
			hit = &mainOnly{"prints a credential's value (--secret)",
				"A leaf gets credentials at launch: report the vault entry name you need to the main session."}
		case kubeCLIs()[name] && prodChange(w, cmd):
			hit = &mainOnly{"runs exec/delete against a production cluster",
				"The main session runs production changes after the owner approves."}
		case (name == "make" || name == "gmake") && isLeaf(in) && reservedMake(w, dir):
			hit = &mainOnly{"runs a reserved make target (audit, install, fmt or one that runs them) in the harness tree",
				"Run `claude-code agents diff --check --path <files>` to check and `claude-code agents fix --path <files>` to " +
					"fix the files that are yours; the main session runs `make` targets from a clean tree."}
		}
		return hit != nil
	})
	return hit
}

// makeDryRun reports a make that only prints or questions its recipe (-n, -q and their long forms).
func makeDryRun(w []string) bool {
	if slices.ContainsFunc(w[1:], makeRunsText) { // another makefile or evaluated text can run commands under -n
		return false
	}
	return slices.ContainsFunc(w[1:], func(a string) bool {
		return slices.Contains([]string{"-n", "--just-print", "--dry-run", "--recon", "-q", "--question"}, a)
	})
}

// makeRunsText reports an argument that feeds make its own makefile or text to evaluate.
func makeRunsText(a string) bool {
	return strings.Contains(a, "$(shell") || slices.ContainsFunc([]string{"-f", "--file", "--makefile", "-E", "--eval"},
		func(f string) bool { return a == f || strings.HasPrefix(a, f+"=") })
}

// vaultKey reports `vault get --key --secret`, the master key's value, in a claude-code command's HarnessArgs.
func vaultKey(h []string) bool {
	return len(h) > 1 && h[0] == "vault" && h[1] == "get" && shell.BoolFlagOn(h[2:], "key") && shell.BoolFlagOn(h[2:], "secret")
}

// kubeValueFlags take a value (next word, `=`, or attached to a short flag); kubeProdFlags' values name the target.
var (
	kubeValueFlags     = sync.OnceValue(func() []string { return shell.GuardWords("kube_value_flags") })
	kubeProdFlags      = sync.OnceValue(func() []string { return shell.GuardWords("kube_prod_flags") })
	kubeNamespaceKinds = sync.OnceValue(func() []string { return shell.GuardWords("kube_namespace_kinds") })
	envWord            = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?:^|\s)ENV=(\S+)`) })
	kubeconfigWord     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?:^|[\s;&|(])KUBECONFIG=(\S+)`) })
	useContext         = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\b(?:config\s+use-context|oc\s+project)\s+(\S+)`)
	})
)

// prodChange reports kubectl/oc exec, delete or oc rsh whose target names production, or whose context is unknown.
func prodChange(w []string, cmd string) bool {
	verb, flags, args := kubeArgs(w)
	if verb != "exec" && verb != "delete" && (verb != "rsh" || filepath.Base(w[0]) != "oc") {
		return false
	}
	if slices.ContainsFunc(prodValues(w, cmd, flags, args), prodName) {
		return true
	}
	if len(flags["--context"])+len(flags["--cluster"]) > 0 || useContext().MatchString(cmd) { // an explicit cluster
		return false
	}
	kubeconfig := strings.Join(flags["--kubeconfig"], "")
	if m := kubeconfigWord().FindStringSubmatch(cmd); kubeconfig == "" && m != nil {
		kubeconfig = m[1]
	}
	current := currentContext(kubeconfig)
	return current == "" || prodName(current) // an unknown context is treated as production
}

// prodValues are the names a kubectl/oc command targets: flags, ENV=, KUBECONFIG=, use-context, a namespace.
func prodValues(w []string, cmd string, flags map[string][]string, args []string) []string {
	var values, remote []string
	for _, f := range kubeProdFlags() {
		values = append(values, flags[f]...)
	}
	if i := slices.Index(w, "--"); i >= 0 {
		remote = w[i+1:] // the command run in the pod, not kubectl's
	}
	for _, re := range []*regexp.Regexp{envWord(), kubeconfigWord(), useContext()} {
		for _, m := range re.FindAllStringSubmatch(cmd, -1) {
			if !slices.Contains(remote, "ENV="+m[1]) {
				values = append(values, m[1])
			}
		}
	}
	if len(args) == 0 {
		return values
	}
	kind, name, slash := strings.Cut(args[0], "/")
	switch {
	case !slices.Contains(kubeNamespaceKinds(), kind):
	case slash:
		values = append(values, name)
	default:
		values = append(values, args[1:]...)
	}
	return values
}

// kubeArgs is a kubectl/oc command's verb, each kubeValueFlags flag's values, and the verb's operands before `--`.
func kubeArgs(w []string) (verb string, flags map[string][]string, args []string) {
	flags = map[string][]string{}
	for i := 1; i < len(w) && w[i] != "--"; i++ {
		flag, val, has := strings.Cut(w[i], "=")
		if len(flag) > 2 && flag[0] == '-' && flag[1] != '-' && slices.Contains(kubeValueFlags(), flag[:2]) {
			flag, val, has = flag[:2], w[i][2:], true // -nprod
		}
		switch {
		case !strings.HasPrefix(flag, "-") && verb == "":
			verb = w[i]
		case !strings.HasPrefix(flag, "-"):
			args = append(args, w[i])
		case !slices.Contains(kubeValueFlags(), flag):
		case !has && i+1 < len(w):
			i, val = i+1, w[i+1]
			fallthrough
		default:
			flags[flag] = append(flags[flag], val)
		}
	}
	return verb, flags, args
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

// reservedMakeTargets write or ship the tree (Makefile: build runs install+fmt+audit, verify audit, bootstrap install).
var reservedMakeTargets = sync.OnceValue(func() []string { return shell.GuardWords("reserved_make_targets") })

// installTargets replace the live install, so a leaf may not run them from a linked worktree of the harness either.
var installTargets = sync.OnceValue(func() []string { return shell.GuardWords("install_make_targets") })

// reservedMake reports a reserved target (or a fixing `lint-<kind>`) whose make dir is the harness tree.
func reservedMake(w []string, dir string) bool {
	if makeDryRun(w) {
		return false
	}
	targets, dir, check := makeArgs(w, dir)
	harness := shell.UnknownDir(dir) || paths.InsideDir(dir, paths.ConfigDir())
	for _, t := range targets {
		lintFix := strings.HasPrefix(t, "lint-") && !check // lint-<kind> fixes files unless CHECK=1
		if harness && (slices.Contains(reservedMakeTargets(), t) || lintFix) ||
			slices.Contains(installTargets(), t) && harnessWorktree(dir) {
			return true
		}
	}
	return false
}

// makeArgs is a make command's targets, its directory (cwd moved by -C) and whether CHECK=1 is set; -n stays a run.
func makeArgs(w []string, dir string) (targets []string, at string, check bool) {
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
		case a == "CHECK=1":
			check = true
		case !strings.HasPrefix(a, "-") && !strings.Contains(a, "="):
			targets = append(targets, a)
		}
	}
	return targets, dir, check
}

// harnessWorktree reports dir inside a linked worktree whose .git file points into the harness repository.
func harnessWorktree(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			text, err := os.ReadFile(filepath.Join(d, ".git"))
			gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(text)), "gitdir: ")
			return !fi.IsDir() && err == nil && ok && paths.InsideDir(gitdir, filepath.Join(paths.ConfigDir(), ".git"))
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}
