package shell

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// maxNest bounds substitution/`bash -c`/`eval` recursion so a pathological command cannot loop.
const maxNest = 4

// segWalk is one eachGit pass over a command's segments.
type segWalk struct {
	cwdTracker
	segs, seps []string
	words      [][]string
	wordsSet   []bool
	fn         gitFn
	depth      int
	runs       bool
	inertBody  []bool // segments that are the body of a heredoc only pure readers consume
}

func newSegWalk(segs, seps []string) *segWalk {
	return &segWalk{segs: segs, seps: seps, words: make([][]string, len(segs)), wordsSet: make([]bool, len(segs))}
}

func (w *segWalk) cmdWords(i int) []string {
	if !w.wordsSet[i] {
		w.words[i] = commandWords(w.segs[i])
		w.wordsSet[i] = true
	}
	return w.words[i]
}

// run visits segment i: a cd moves the tracker, git words and nested text are reported to fn.
func (w *segWalk) run(i int) bool {
	toks := w.cmdWords(i)
	if len(toks) == 0 {
		return false
	}
	prev := ""
	if i > 0 {
		prev = w.seps[i-1]
	}
	at, isCd := w.step(toks, prev, w.seps[i])
	if isCd {
		return false
	}
	rest, dir := Unwrap(toks, at)
	fn := w.fn
	if w.heredocText(i) {
		fn = func(target, verb string, args []string) bool {
			return verb != commandVerb && w.fn(target, verb, args)
		}
	}
	if segmentGit(rest, dir, fn, w.depth, w.runs || w.pipesIntoRunner(i) || w.writesThenRuns(i)) {
		return true
	}
	if w.pipesIntoShell(i) {
		return pipedIntoShell(rest, dir, w.fn, w.depth, w.runs)
	}
	return false
}

// heredocText reports segment i as the body of a heredoc that only pure readers consume and nothing runs.
func (w *segWalk) heredocText(i int) bool {
	if w.inertBody == nil {
		w.inertBody = w.heredocBodies(true)
	}
	return w.inertBody[i]
}

// HeredocBodies marks the heredoc bodies that are text: only pure readers consume them and no file they write runs.
func HeredocBodies(segs, seps []string) []bool { return newSegWalk(segs, seps).heredocBodies(true) }

// heredocBodies marks each heredoc body that is text (readOnly consumes it), or with text false each live one.
func (w *segWalk) heredocBodies(text bool) []bool {
	out := make([]bool, len(w.segs))
	for j := range w.segs {
		delim, ok := heredocDelim(w.cmdWords(j))
		if !ok || w.readOnly(j) != text {
			continue
		}
		k := j + 1
		for k < len(w.segs) && w.seps[k-1] != "\n" { // the body starts after the line's pipeline
			k++
		}
		for ; k < len(w.segs) && strings.TrimSpace(w.segs[k]) != delim; k++ {
			out[k] = true
		}
	}
	return out
}

// heredocDelim is the delimiter word of a `<<DELIM` or `<<- DELIM` in toks.
func heredocDelim(toks []string) (string, bool) {
	for i, t := range toks {
		switch {
		case t == "<<" || t == "<<-":
			if i+1 < len(toks) {
				return toks[i+1], true
			}
		case strings.HasPrefix(t, "<<") && !strings.HasPrefix(t, "<<<"):
			return strings.TrimLeft(t[2:], "-"), true
		}
	}
	return "", false
}

// pureReaders read, filter or copy their input and never run it (not awk, mysql, sed without -n); readOnly checks tee.
func pureReaders() map[string]bool { walkerOnce.Do(loadWalker); return walker.pureReaders }

// pureReader reports a pureReaders command, `sed -n` with no `e` in its script, or `git commit|tag -F -`.
func pureReader(toks []string) bool {
	switch filepath.Base(toks[0]) {
	case "sed":
		quiet := slices.ContainsFunc(toks[1:], func(t string) bool { return t == "-n" || t == "--quiet" || t == "--silent" })
		return quiet && !slices.ContainsFunc(toks[1:], func(t string) bool { return !strings.HasPrefix(t, "-") && strings.Contains(t, "e") })
	case "git":
		_, verb, args := resolvedGit(toks[1:], "")
		for i, a := range args {
			if a == "-F-" || a == "--file=-" || (a == "-F" || a == "--file") && i+1 < len(args) && args[i+1] == "-" {
				return verb == "commit" || verb == "tag"
			}
		}
		return false
	}
	return pureReaders()[filepath.Base(toks[0])]
}

// readOnly reports segment j's pipeline as pure readers, and no file they write run by a later segment.
func (w *segWalk) readOnly(j int) bool {
	var files []string
	for k := j; ; k++ {
		words, targets := Redirects(w.segs[k])
		for len(words) > 0 && isShellKeyword(words[0]) {
			words = words[1:]
		}
		rest, _ := Unwrap(words, "")
		if rest = DropRtk(rest); len(rest) == 0 || !pureReader(rest) {
			return false
		}
		if filepath.Base(rest[0]) == "tee" {
			targets = append(targets, operandWords(rest[1:])...)
		}
		files = append(files, targets...)
		if k+1 >= len(w.segs) || w.seps[k] != "|" && w.seps[k] != "|&" {
			return len(files) == 0 || !w.runsFile(files, k+1)
		}
	}
}

// runsFile reports a segment from index from on that runs one of files: by path, or an evaluator naming it.
func (w *segWalk) runsFile(files []string, from int) bool {
	names := map[string]bool{}
	for _, f := range files {
		for _, p := range pathPieces(f) {
			names[filepath.Base(p)] = true
		}
	}
	for k := from; k < len(w.segs); k++ {
		toks := w.cmdWords(k)
		rest, _ := Unwrap(toks, "")
		if rest = DropRtk(rest); len(rest) > 0 && strings.Contains(rest[0], "/") && names[filepath.Base(rest[0])] {
			return true
		}
		piped := (w.seps[k] == "|" || w.seps[k] == "|&") && k+1 < len(w.segs) && evaluates(w.cmdWords(k+1))
		if !piped && !evaluates(toks) {
			continue
		}
		for _, t := range toks {
			if slices.ContainsFunc(pathPieces(t), func(p string) bool { return names[filepath.Base(p)] }) {
				return true
			}
		}
	}
	return false
}

// pathPieces splits a word into its path-like runs: `<s`, `$(cat ./s)` and `"$F"` hold s, ./s and F.
func pathPieces(t string) []string {
	return strings.FieldsFunc(t, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._-/~", r)
	})
}

// evaluators read text as code: a string, a file or stdin; shells and interpreterName count too.
func evaluators() map[string]bool { walkerOnce.Do(loadWalker); return walker.evaluators }

// interpreterName is an interpreter's base name with an optional version: python3.12, pypy3, node, bun, deno, php5.
var interpreterName = regexp.MustCompile(`^(?:python|pypy|perl|ruby|node|nodejs|bun|deno|php|lua|luajit|fish|tclsh|` +
	`osascript|Rscript|pwsh)[0-9.]*$`)

// evaluator reports a command word that reads text as code, or a dynamic one (`$CMD`, `$(…)`) whose text is unknown.
func evaluator(word string) bool {
	name := filepath.Base(word)
	return evaluators()[name] || shellWrapperNames()[name] || interpreterName.MatchString(name) ||
		strings.HasPrefix(word, "$") || strings.HasPrefix(word, "`")
}

// evaluatorStart is the first command start in toks that is an evaluator.
func evaluatorStart(toks []string) ([]string, bool) {
	for _, st := range CommandStarts(toks) {
		if st = skipEnvAssignments(st); len(st) > 0 && evaluator(st[0]) {
			return st, true
		}
	}
	return nil, false
}

func evaluates(toks []string) bool { _, ok := evaluatorStart(toks); return ok }

// EvaluatedText is cmd's text an evaluator runs: its words, a live heredoc body, a pipe's left side, a script run.
func EvaluatedText(cmd, cwd string) []string {
	cmd = DropComments(cmd)
	var out []string
	for _, s := range RunScripts(cmd, cwd) {
		out = append(out, s.Text)
	}
	_, subs := extractSubstitutions(cmd, subMask)
	for _, sub := range subs {
		out = append(out, EvaluatedText(sub, cwd)...)
	}
	segs, seps := SegmentsSep(cmd)
	w := newSegWalk(segs, seps)
	text, live := w.heredocBodies(true), w.heredocBodies(false)
	for i, seg := range segs {
		toks := Tokens(seg) // not commandWords: an ssh command evaluates its string remotely
		if len(toks) > 0 {
			toks[0] = strings.TrimLeft(toks[0], "({`")
		}
		st, ok := evaluatorStart(toks)
		switch {
		case live[i] || !text[i] && w.pipesIntoEvaluator(i):
			out = append(out, seg)
		case ok && !text[i]:
			out = append(out, strings.Join(st, " "))
		}
	}
	return out
}

// pipesIntoEvaluator reports segment i feeding, through the rest of its pipeline, an evaluator's stdin.
func (w *segWalk) pipesIntoEvaluator(i int) bool {
	for k := i; k+1 < len(w.segs) && (w.seps[k] == "|" || w.seps[k] == "|&"); k++ {
		if evaluates(w.cmdWords(k + 1)) {
			return true
		}
	}
	return false
}

// pipesIntoRunner reports that segment i feeds a runner (xargs, eval, a shell with a script) its words to run.
func (w *segWalk) pipesIntoRunner(i int) bool {
	if w.seps[i] != "|" && w.seps[i] != "|&" || i+1 >= len(w.segs) {
		return false
	}
	next := w.cmdWords(i + 1)
	rest, _ := Unwrap(next, "")
	return len(rest) > 0 && runner(rest[0]) && !bareShell(next) // a bare shell's input is pipedIntoShell's
}

// writesThenRuns reports segment i writing a file while a later segment runs a script or a command by path.
func (w *segWalk) writesThenRuns(i int) bool {
	if !FileRedirect(w.segs[i]) {
		return false
	}
	for j := i + 1; j < len(w.segs); j++ {
		rest, _ := Unwrap(w.cmdWords(j), "")
		if len(rest) > 1 && runner(rest[0]) || len(rest) > 0 && strings.Contains(rest[0], "/") {
			return true
		}
	}
	return false
}

// runArgs run an argument as a command line or script; shellWrapperNames do too.
func runArgs() map[string]bool { walkerOnce.Do(loadWalker); return walker.runArgs }

func runner(name string) bool {
	return runArgs()[filepath.Base(name)] || shellWrapperNames()[filepath.Base(name)]
}

// executedSubs marks each substitution whose output runs: a command word, a runner's argument, or a var it expands.
func executedSubs(masked string, n int) []bool {
	out := make([]bool, n)
	var assigned []int
	varRun, k := false, 0
	for _, seg := range Segments(masked) {
		toks := Tokens(seg)
		kw := 0
		for kw < len(toks) && isShellKeyword(toks[kw]) {
			kw++
		}
		rest, _ := Unwrap(toks[kw:], "")
		at := max(kw, len(toks)-len(rest))
		for j, t := range toks {
			code := j == at || j > at && len(rest) > 0 && runner(rest[0])
			varRun = varRun || code && strings.Contains(t, "$")
			for range strings.Count(t, subMask) {
				switch {
				case k >= n:
				case code:
					out[k] = true
				case j < at && isEnvAssign(t):
					assigned = append(assigned, k)
				}
				k++
			}
		}
	}
	for _, a := range assigned {
		out[a] = out[a] || varRun
	}
	return out
}

// pipesIntoShell reports that segment i feeds a bare shell that reads its commands from stdin.
func (w *segWalk) pipesIntoShell(i int) bool {
	return (w.seps[i] == "|" || w.seps[i] == "|&") && i+1 < len(w.segs) && bareShell(w.cmdWords(i+1)) &&
		!w.casePattern(i)
}

// casePatternHead matches the segment `case WORD in PATTERN` that opens a case pattern list.
var casePatternHead = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\bcase\s.*\sin\s+\S+$`) })

// casePattern reports the `|` after segment i as a case pattern separator (`in sh|bash) …`), not a pipe.
func (w *segWalk) casePattern(i int) bool {
	head, last := i, i+1
	for head > 0 && w.seps[head-1] == "|" {
		head--
	}
	for last < len(w.segs)-1 && w.seps[last] == "|" {
		last++
	}
	bare := func(s string) bool { return s != "" && !strings.ContainsAny(strings.TrimSpace(s), " \t") }
	opens := head == 0 || w.seps[head-1] == ";;" || w.seps[head-1] == "\n"
	if h := strings.TrimSpace(w.segs[head]); !casePatternHead().MatchString(h) && (!opens || !bare(h)) {
		return false
	}
	for _, s := range w.segs[head+1 : last] {
		if !bare(s) {
			return false
		}
	}
	before, _, ok := strings.Cut(w.segs[last], ")")
	return ok && bare(before)
}

// dirExists is the seam for "does this cd target exist"; a cd to a missing directory fails and leaves cwd alone.
var dirExists = statDir

func statDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// unknownDir is the cwd reported after a cd whose target is dynamic: absolute, never a real dir or under one.
var unknownDir = filepath.Join(string(filepath.Separator), "<unknown cwd>")

// UnknownDir reports the target EachGit gives git run after a dynamic cd in the same `&&` chain.
func UnknownDir(dir string) bool { return dir == unknownDir }

// cwdTracker follows the shell cwd across subshells, pipes, `||` after a cd, missing targets and pushd/popd.
type cwdTracker struct {
	cwdState
	before   string // the dir the previous segment's cd left, for a following `||`
	start    string // the dir the command started in, set by the first step
	moved    bool
	subshell []cwdState      // the state to restore at each open `(`'s close
	dirs     []string        // pushd stack
	made     map[string]bool // dirs an earlier mkdir in this command creates, so a cd into one moves the cwd
}

// cwdState is what a subshell's close restores.
type cwdState struct {
	cwd    string
	exact  string // after a dynamic or missing cd target, the dir for the rest of its `&&` chain
	lost   bool   // a dynamic cd took effect: cwd is only the starting dir's guess
	oldpwd string // where `cd -` goes; "" until a cd runs
}

// here is the dir the next segment runs in.
func (c *cwdTracker) here() string { return cmp.Or(c.exact, c.cwd) }

func (c *cwdTracker) enter(seg string) {
	opens, _ := parenCounts(seg)
	for range opens {
		c.subshell = append(c.subshell, c.cwdState)
	}
}

func (c *cwdTracker) leave(seg string) {
	_, closes := parenCounts(seg)
	for range closes {
		if n := len(c.subshell); n > 0 {
			c.cwdState, c.subshell = c.subshell[n-1], c.subshell[:n-1]
			c.moved = false
		}
	}
}

// EachSegmentDir calls fn per non-cd segment with its dir, tracked as EachGit does; lost: a dynamic cd moved it.
func EachSegmentDir(cmd, cwd string, fn func(seg string, toks []string, dir string, lost bool)) {
	segs, seps := SegmentsSep(cmd)
	eachSegment(segs, seps, cwd, func(i int, toks []string, dir string, lost bool) { fn(segs[i], toks, dir, lost) })
}

// eachSegment is EachSegmentDir over split segments, passing each segment's index.
func eachSegment(segs, seps []string, cwd string, fn func(i int, toks []string, dir string, lost bool)) {
	c := &cwdTracker{cwd: cwd}
	for i, seg := range segs {
		c.enter(seg)
		toks, prev := Tokens(seg), ""
		if i > 0 {
			prev = seps[i-1]
		}
		words := toks
		for len(words) > 0 && isShellKeyword(words[0]) {
			words = words[1:]
		}
		if at, isCd := c.step(words, prev, seps[i]); !isCd && len(toks) > 0 {
			lost := c.lost || at == unknownDir
			if at == unknownDir {
				at = c.cwd
			}
			fn(i, toks, at, lost)
		}
		c.leave(seg)
	}
}

// step returns a segment's dir and applies a builtin cd/pushd/popd, except on a pipe side or backgrounded segment.
func (c *cwdTracker) step(toks []string, prev, sep string) (at string, isCd bool) {
	if prev != "&&" && prev != "|" && prev != "|&" { // a failed cd no longer stops what follows
		c.exact = ""
	}
	at = c.here()
	if prev == "||" && c.moved {
		at = c.before
	}
	c.moved = false
	if c.start == "" {
		c.start = c.cwd
	}
	cmd := c.dashResolved(builtinPrefixDropped(toks))
	c.noteMkdir(cmd)
	next, exact, ok := c.cdTarget(cmd, !subshellSide(prev, sep))
	if ok && !subshellSide(prev, sep) {
		abs, dynamic, _ := cdResolve(cmd, "")
		c.lost = exact == unknownDir || c.lost && (dynamic || !filepath.IsAbs(abs))
		c.oldpwd = at
	}
	if ok {
		c.apply(next, exact, prev, sep)
	}
	return at, ok
}

// dashResolved turns `cd -` into a cd to the previous dir; an unknown one is the starting dir, the conservative side.
func (c *cwdTracker) dashResolved(cmd []string) []string {
	if len(cmd) == 0 || cmd[0] != "cd" {
		return cmd
	}
	if args := skipCdOptions(cmd[1:]); len(args) == 0 || args[0] != "-" {
		return cmd
	}
	prev := c.oldpwd
	if prev == "" || prev == unknownDir {
		prev = c.start
	}
	return []string{"cd", prev}
}

// cdTarget is the cwd a cd/pushd/popd leaves (a missing dir: cwd stays) and exact, where it lands when it succeeds.
func (c *cwdTracker) cdTarget(cmd []string, live bool) (next, exact string, ok bool) {
	if len(cmd) > 0 && cmd[0] == "popd" {
		d := popDir(&c.dirs, c.cwd)
		return d, d, true
	}
	next, ok = CdTarget(cmd, c.cwd)
	if !ok {
		return c.cwd, c.here(), false
	}
	exact, dynamic, _ := cdResolve(cmd, c.here())
	if dynamic {
		exact = unknownDir
	}
	if missingDir(next, c.cwd) && !c.made[next] {
		next = c.cwd
	}
	if live && next != c.cwd && cmd[0] == "pushd" {
		c.dirs = append(c.dirs, c.cwd)
	}
	return next, exact, true
}

// noteMkdir records each static dir a mkdir creates, with its parents.
func (c *cwdTracker) noteMkdir(cmd []string) {
	if len(cmd) == 0 || cmd[0] != "mkdir" {
		return
	}
	for _, a := range cmd[1:] {
		if strings.HasPrefix(a, "-") || strings.ContainsAny(a, "$`*?~") {
			continue
		}
		d := a
		if !filepath.IsAbs(d) {
			d = filepath.Join(c.here(), d)
		}
		if c.made == nil {
			c.made = map[string]bool{}
		}
		for d = filepath.Clean(d); filepath.IsAbs(d) && d != filepath.Dir(d); d = filepath.Dir(d) {
			c.made[d] = true
		}
	}
}

// missingDir reports a cd target that is absolute, differs from cwd and does not exist.
func missingDir(next, cwd string) bool {
	return filepath.IsAbs(next) && next != cwd && !dirExists(next)
}

// apply moves the cwd unless this segment is a pipe side or backgrounded.
func (c *cwdTracker) apply(next, exact, prev, sep string) {
	if subshellSide(prev, sep) {
		return
	}
	if next != c.cwd {
		c.before, c.cwd, c.moved = c.cwd, next, true
	}
	c.exact = ""
	if exact != next {
		c.exact = exact
	}
}

func subshellSide(prev, sep string) bool {
	return prev == "|" || prev == "|&" || sep == "|" || sep == "|&" || sep == "&"
}

func popDir(dirs *[]string, cwd string) string {
	n := len(*dirs)
	if n == 0 {
		return cwd
	}
	d := (*dirs)[n-1]
	*dirs = (*dirs)[:n-1]
	return d
}

// parenCounts counts the unquoted `(` and `)` in a segment (substitutions are already masked out).
func parenCounts(seg string) (opens, closes int) {
	if !strings.ContainsAny(seg, "()") {
		return 0, 0
	}
	var quote byte
	for i := 0; i < len(seg); i++ {
		if quote != 0 {
			i, quote = quotedStep(seg, i, quote)
			continue
		}
		switch seg[i] {
		case '\\':
			i++
		case '\'', '"':
			quote = seg[i]
		case '(':
			opens++
		case ')':
			closes++
		}
	}
	return opens, closes
}

// builtinPrefixDropped drops NAME=val, `command`, `builtin`, `time`: prefixes whose cd still moves this shell.
func builtinPrefixDropped(toks []string) []string {
	for {
		toks = skipEnvAssignments(toks)
		if len(toks) == 0 || toks[0] != "command" && toks[0] != "builtin" && toks[0] != "time" {
			return toks
		}
		toks, _ = skipWrapperOpts(toks[0], toks[1:], "")
	}
}

// commandWords is the segment's words without leading shell keywords; nil for an ssh command (it runs remotely).
func commandWords(seg string) []string {
	toks := Tokens(seg)
	for len(toks) > 0 && isShellKeyword(toks[0]) {
		toks = toks[1:]
	}
	if len(toks) > 0 && toks[0] == "ssh" {
		return nil
	}
	return toks
}

// CommandStarts lists the words from each command word of a segment, through keywords, wrappers, runners, find -exec.
func CommandStarts(toks []string) [][]string { return commandStarts(toks, maxNest*2) }

func commandStarts(toks []string, n int) [][]string {
	for len(toks) > 0 && isShellKeyword(toks[0]) {
		toks = toks[1:]
	}
	if len(toks) == 0 || n == 0 {
		return nil
	}
	out := [][]string{toks}
	if filepath.Base(toks[0]) != "find" {
		return append(out, commandStarts(afterPrefix(skipEnvAssignments(toks)), n-1)...)
	}
	for i, t := range toks { // every -exec runs its own command
		if slices.Contains(execFlags(), t) {
			out = append(out, commandStarts(toks[i+1:], n-1)...)
		}
	}
	return out
}

// execFlags are find's flags that run the words after them as a command.
func execFlags() []string { walkerOnce.Do(loadWalker); return walker.execFlags }

// runnerValueFlags take the next word as their value in xargs, parallel and watch.
func runnerValueFlags() []string { walkerOnce.Do(loadWalker); return walker.runnerValueFlags }

// goRunValueFlags take the next word as their value in `go run`.
func goRunValueFlags() []string { walkerOnce.Do(loadWalker); return walker.goRunValueFlags }

// optionsDropped drops leading options, the value of each one in values, and a `--` that ends them.
func optionsDropped(rest, values []string) []string {
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		opt := rest[0]
		if rest = rest[1:]; opt == "--" {
			break
		}
		if slices.Contains(values, opt) && len(rest) > 0 {
			rest = rest[1:]
		}
	}
	return rest
}

// afterPrefix is the words after a leading wrapper (rtk, env, sudo), runner (xargs, parallel, watch) or `go run`.
func afterPrefix(toks []string) []string {
	if len(toks) == 0 {
		return nil
	}
	switch name := filepath.Base(toks[0]); {
	case name == "rtk":
		return DropRtk(toks)
	case wrapperNames()[name]:
		rest, _ := skipWrapperOpts(name, toks[1:], "")
		if wrapperOperands()[name] && len(rest) > 0 {
			rest = rest[1:]
		}
		if name == "env" && len(rest) > 0 && rest[0] == "-" {
			rest = rest[1:]
		}
		return rest
	case name == "xargs" || name == "parallel" || name == "watch":
		return optionsDropped(toks[1:], runnerValueFlags())
	case name == "go" && len(toks) > 1 && toks[1] == "run":
		return optionsDropped(toks[2:], goRunValueFlags())
	case name == "caffeinate" || name == "nsenter": // nsenter runs the command in this env, in other namespaces
		return optionsDropped(toks[1:], []string{"-t", "-w", "-S", "-G"})
	case name == "busybox": // the applet name is the command
		return toks[1:]
	}
	return nil
}

// rtkRunners are the rtk subcommands that run the command after them; the others (git, grep, read) are the command.
func rtkRunners() map[string]bool { walkerOnce.Do(loadWalker); return walker.rtkRunners }

// DropRtk drops a leading `rtk`, its flags and a runner subcommand, so `rtk grep` reads as `grep`, `rtk err x` as `x`.
func DropRtk(toks []string) []string {
	if len(toks) == 0 || filepath.Base(toks[0]) != "rtk" {
		return toks
	}
	flagsDropped := func(t []string) []string {
		for len(t) > 0 && strings.HasPrefix(t[0], "-") {
			t = t[1:]
		}
		return t
	}
	toks = flagsDropped(toks[1:])
	if len(toks) > 0 && rtkRunners()[toks[0]] {
		toks = flagsDropped(toks[1:])
	}
	return toks
}

// cStringCommands run the string after their -c flag through a shell.
func cStringCommands() map[string]bool { walkerOnce.Do(loadWalker); return walker.cString }

func isCFlag(t string) bool {
	return len(t) > 1 && t[0] == '-' && t[1] != '-' && strings.ContainsRune(t[1:], 'c')
}

// bareShell reports a shell with no -c string and no script file: it reads its commands from stdin.
func bareShell(toks []string) bool {
	rest, _ := Unwrap(toks, "")
	if len(rest) == 0 || !shellWrapperNames()[filepath.Base(rest[0])] {
		return false
	}
	if _, ok := nestedOf(rest); ok {
		return false
	}
	for _, t := range rest[1:] {
		if !strings.HasPrefix(t, "-") && !strings.ContainsAny(t, "<>") {
			return false
		}
	}
	return true
}

// Script is text a command line writes to a file with echo/printf (a redirect or tee) and then runs from that file.
type Script struct {
	Text, Dir string
	Lost      bool // a dynamic cd moved the run: Dir is only the starting dir's guess
}

// RunScripts lists each file cmd writes from printed text that a later segment runs (`sh f`, `. f`, `./f`).
func RunScripts(cmd, cwd string) []Script {
	if !strings.Contains(cmd, ">") && !strings.Contains(cmd, "tee") {
		return nil
	}
	segs, seps := SegmentsSep(cmd)
	return runScripts(segs, seps, cwd)
}

func runScripts(segs, seps []string, cwd string) []Script {
	written := map[string][]string{}
	var piped, out []string
	var scripts []Script
	eachSegment(segs, seps, cwd, func(i int, words []string, dir string, lost bool) {
		words, targets := redirectsTokens(segs[i], words)
		for len(words) > 0 && isShellKeyword(words[0]) {
			words = words[1:]
		}
		rest, _ := Unwrap(words, dir)
		if len(rest) == 0 {
			return
		}
		out, piped = printed(rest, piped), nil
		if filepath.Base(rest[0]) == "tee" {
			targets = append(targets, operandWords(rest[1:])...)
		}
		for _, t := range targets {
			k := scriptKey(t, dir)
			written[k] = append(written[k], out...)
		}
		if f, ok := scriptRun(rest); ok {
			for _, text := range written[scriptKey(f, dir)] {
				scripts = append(scripts, Script{Text: text, Dir: dir, Lost: lost})
			}
		}
		if seps[i] == "|" || seps[i] == "|&" {
			piped = out
		}
	})
	return scripts
}

// printed is what a segment prints: echo's text raw and with escapes decoded, printf's formatted, tee's stdin.
func printed(toks []string, stdin []string) []string {
	words := toks[1:]
	switch filepath.Base(toks[0]) {
	case "tee":
		return stdin
	case "echo":
		for len(words) > 0 && len(words[0]) > 1 && words[0][0] == '-' && strings.Trim(words[0][1:], "neE") == "" {
			words = words[1:]
		}
		raw := strings.Join(words, " ")
		return slices.Compact([]string{raw, decodeEscapes(raw)})
	case "printf":
		if len(words) > 0 && words[0] == "--" {
			words = words[1:]
		}
		if len(words) == 0 || strings.HasPrefix(words[0], "-") { // -v assigns a variable, printing nothing
			return nil
		}
		return []string{printfOutput(words[0], words[1:])}
	}
	return nil
}

// operandWords are the words of args that are not options.
func operandWords(args []string) []string {
	var out []string
	for _, a := range args {
		if a != "" && a[0] != '-' {
			out = append(out, a)
		}
	}
	return out
}

// scriptKey names the file t at dir: its path, or the word itself when the name or the dir is dynamic.
func scriptKey(t, dir string) string {
	if p, ok := resolvePath(t, dir); ok && p != unknownDir {
		return p
	}
	return t
}

// scriptRun is the file a segment runs as a script: a shell's first operand, `.`/`source`'s, or a command by path.
func scriptRun(toks []string) (string, bool) {
	name := filepath.Base(toks[0])
	switch {
	case shellWrapperNames()[name]:
		if _, ok := nestedOf(toks); ok {
			return "", false
		}
		if ops := operandWords(toks[1:]); len(ops) > 0 {
			return ops[0], true
		}
		return "", false
	case (name == "." || name == "source") && len(toks) > 1:
		return toks[1], true
	}
	return toks[0], strings.Contains(toks[0], "/")
}

// printfOutput applies a printf format to args, reusing the format while args remain, as the shell does.
func printfOutput(format string, args []string) string {
	var out strings.Builder
	for {
		used := printfPass(&out, format, args)
		if args = args[used:]; used == 0 || len(args) == 0 {
			return out.String()
		}
	}
}

// printfPass writes one pass of format: its escapes decoded, each % verb an argument (`%b` decoded); returns args used.
func printfPass(out *strings.Builder, format string, args []string) int {
	used := 0
	for i := 0; i < len(format); i++ {
		switch {
		case format[i] == '\\' && i+1 < len(format):
			i += writeEscape(out, format[i+1:], false)
		case format[i] == '%' && i+1 < len(format):
			j := i + 1
			for j < len(format) && strings.IndexByte("-+ #0123456789.", format[j]) >= 0 {
				j++
			}
			i = min(j, len(format)-1)
			if j >= len(format) || format[j] == '%' {
				out.WriteByte('%')
				continue
			}
			arg := ""
			if used < len(args) {
				arg, used = args[used], used+1
			}
			if format[j] == 'b' {
				arg = decodeEscapes(arg)
			}
			out.WriteString(arg)
		default:
			out.WriteByte(format[i])
		}
	}
	return used
}

// decodeEscapes applies echo -e / printf %b escapes: `\0NNN` octal, and `\c` ends the output.
func decodeEscapes(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		if s[i+1] == 'c' {
			break
		}
		i += writeEscape(&out, s[i+1:], true)
	}
	return out.String()
}

// writeEscape writes the escape after a backslash (s) and returns its length; zeroOctal reads `\0NNN` as one byte.
func writeEscape(out *strings.Builder, s string, zeroOctal bool) int {
	if zeroOctal && s[0] == '0' {
		end := digitRun(s, 1, 3, 8)
		v, _ := strconv.ParseUint("0"+s[1:max(end, 1)], 8, 8)
		out.WriteByte(byte(v))
		return max(end, 1)
	}
	if r, n, ok := ansiEscape(s); ok {
		out.WriteString(r)
		return n
	}
	out.WriteByte('\\')
	out.WriteByte(s[0])
	return 1
}

// Expands reports a backtick, `$(`, `${` or `$NAME` outside single quotes; a trailing `$` is literal.
func Expands(seg string) bool {
	var q byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case q == '\'':
			if c == '\'' {
				q = 0
			}
		case c == '\\':
			i++
		case c == '`':
			return true
		case c == '$':
			if i+1 == len(seg) || seg[i+1] == ' ' || seg[i+1] == '\t' || seg[i+1] == '\n' || (q == '"' && seg[i+1] == '"') {
				continue
			}
			return true
		case q == 0 && (c == '\'' || c == '"'):
			q = c
		case q == '"' && c == '"':
			q = 0
		}
	}
	return false
}
