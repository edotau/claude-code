// Ported from rtk v1.1.0 src/discover/registry.rs, the rewrite_command path (Apache 2.0, github.com/rtk-ai/rtk).
package rtkwrap

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// rctx is the pipeline position a segment is rewritten in.
type rctx uint8

const (
	ctxNormal rctx = iota
	ctxFinal
	ctxProducer
)

// maxPrefixDepth bounds nested wrapper peels; a wrapper's script may be rewritten one level deep only.
const maxPrefixDepth = 10

// rewriter holds rtk's exclude_commands and marks unsure when the port meets logic it does not carry.
type rewriter struct {
	excl   []string
	unsure bool
}

// rewriteCommand is `rtk rewrite`'s stdout for cmd ("" when it defers); certain is false where the port bails.
func rewriteCommand(cmd string, excl []string) (out string, certain bool) {
	if unattestable(cmd, posix) { // rtk's hook decision defers what its permission gate cannot decompose
		return "", true
	}
	r := &rewriter{excl: excl}
	out, ok := r.top(cmd)
	if !ok {
		out = ""
	}
	return out, !r.unsure
}

func (r *rewriter) top(cmd string) (string, bool) {
	px := prefixRx()
	if strings.Contains(cmd, `\`) { // a continuation can split << or $(( that the space-join would hide
		joined := px["bashJoin"].ReplaceAllString(cmd, "")
		if hasHeredoc(joined) || strings.Contains(joined, "$((") {
			return "", false
		}
	}
	t := strings.TrimSpace(px["lineCont"].ReplaceAllString(cmd, " "))
	if t == "" || hasHeredoc(t) || strings.Contains(t, "$((") {
		return "", false
	}
	if strings.Contains(t, "\n") {
		return r.multiline(t)
	}
	return r.single(t)
}

func (r *rewriter) single(t string) (string, bool) {
	compound := strings.ContainsAny(t, ";|") || strings.Contains(t, "&&") || strings.Contains(t, " & ")
	if !compound && (strings.HasPrefix(t, "rtk ") || t == "rtk") {
		return t, true
	}
	return r.compound(t, 0)
}

type lineRole uint8

const (
	rolePassive lineRole = iota
	roleIndependent
	roleContinues
	roleUnsafe
)

// lineSeg is one physical line of a block and its byte offset.
type lineSeg struct {
	off  int
	text string
}

func (r *rewriter) multiline(cmd string) (string, bool) {
	var offs []int
	for _, t := range tokenizeMode(cmd, nlBash) {
		if t.kind == tkOp && t.val == "\n" {
			offs = append(offs, t.off)
		}
	}
	if ansiCDefeatsLexer(cmd) {
		return "", false
	}
	if rawBreaks(cmd) != len(offs) { // a newline swallowed by quotes: one logical command, or unreadable
		if len(offs) == 0 && quoteScan(cmd, func(int, byte, bool, bool) bool { return true }) {
			return r.single(cmd)
		}
		return "", false
	}
	segs := make([]lineSeg, 0, len(offs)+1)
	roles := make([]lineRole, 0, len(offs)+1)
	start := 0
	for _, off := range append(offs, len(cmd)) {
		segs = append(segs, lineSeg{start, cmd[start:off]})
		if roles = append(roles, classifyLine(strings.TrimSpace(cmd[start:off]))); roles[len(roles)-1] == roleUnsafe {
			return "", false
		}
		start = off + 1
	}
	return r.joinLines(cmd, offs, segs, roles)
}

func rawBreaks(cmd string) int {
	n := strings.Count(cmd, "\n")
	for i := range len(cmd) {
		if crlfAt(cmd, i) {
			n++
		}
	}
	return n
}

func (r *rewriter) joinLines(cmd string, offs []int, segs []lineSeg, roles []lineRole) (string, bool) {
	var b strings.Builder
	changed := false
	for i := 0; i < len(segs); {
		if i > 0 {
			b.WriteString(cmd[offs[i-1] : offs[i-1]+1])
		}
		seg := segs[i]
		if roles[i] == rolePassive {
			b.WriteString(seg.text)
			i++
			continue
		}
		end, ok := continuedEnd(segs, roles, i)
		if !ok {
			return "", false
		}
		unit := cmd[seg.off : segs[end].off+len(segs[end].text)]
		line := strings.TrimSpace(unit)
		if out, ok := r.single(line); ok && out != line {
			changed = true
			b.WriteString(seg.text[:len(seg.text)-len(strings.TrimLeftFunc(seg.text, unicode.IsSpace))])
			b.WriteString(out)
		} else {
			b.WriteString(unit)
		}
		i = end + 1
	}
	return b.String(), changed
}

// continuedEnd is the last line of the unit starting at i; ok false when a comment line sits inside it.
func continuedEnd(segs []lineSeg, roles []lineRole, i int) (int, bool) {
	end := i
	for roles[end] == roleContinues {
		next := end + 1
		for next < len(segs) && strings.TrimSpace(segs[next].text) == "" {
			next++
		}
		if next >= len(segs) {
			break
		}
		if roles[next] == rolePassive {
			return 0, false
		}
		end = next
	}
	return end, true
}

var continuationOps = []string{"&&", "||", "|&", "|"}

func classifyLine(line string) lineRole {
	if line == "" || strings.HasPrefix(line, "#") {
		return rolePassive
	}
	cs := commentStart(line)
	code := line
	if cs >= 0 {
		code = strings.TrimRightFunc(line[:cs], unicode.IsSpace)
	}
	switch {
	case lineUnsafe(code):
		return roleUnsafe
	case !hasAny(code, continuationOps, strings.HasSuffix):
		return roleIndependent
	case cs >= 0: // an operator behind a comment cannot be joined textually
		return roleUnsafe
	}
	return roleContinues
}

// lineUnsafe: a block keyword, a leading operator, (( )) or brackets that span lines make the block pass through.
func lineUnsafe(code string) bool {
	if f := strings.Fields(code); len(f) > 0 && slices.Contains(blockKeywords, f[0]) {
		return true
	}
	return hasAny(code, continuationOps, strings.HasPrefix) || strings.HasPrefix(code, "((") ||
		strings.HasSuffix(code, "))") || unbalancedGrouping(code) || unbalancedTest(code)
}

func hasAny(s string, list []string, has func(string, string) bool) bool {
	for _, p := range list {
		if has(s, p) {
			return true
		}
	}
	return false
}

func isASCIISpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\f' || b == '\r' }

// commentStart is where an unquoted # opening a word starts a comment, -1 when none (`${#v}` is no comment).
func commentStart(line string) int {
	at := -1
	quoteScan(line, func(i int, b byte, sq, dq bool) bool {
		if b == '#' && !sq && !dq && (i == 0 || isASCIISpace(line[i-1]) || strings.IndexByte("|&;()", line[i-1]) >= 0) {
			at = i
			return false
		}
		return true
	})
	return at
}

func unbalancedGrouping(code string) bool {
	paren, brace, bad := 0, 0, false
	quoteScan(code, func(_ int, b byte, sq, dq bool) bool {
		if sq || dq {
			return true
		}
		paren += delta(b, '(', ')')
		brace += delta(b, '{', '}')
		bad = paren < 0 || brace < 0
		return !bad
	})
	return bad || paren != 0 || brace != 0
}

func delta(b, open, closing byte) int {
	switch b {
	case open:
		return 1
	case closing:
		return -1
	}
	return 0
}

func unbalancedTest(code string) bool {
	depth := 0
	quoteScan(code, func(i int, b byte, sq, dq bool) bool {
		if !sq && !dq && testBracketWord(code, i) {
			depth += delta(b, '[', ']')
		}
		return depth >= 0
	})
	return depth != 0
}

// testBracketWord reports a whole-word [[ or ]] starting at i.
func testBracketWord(code string, i int) bool {
	b := code[i]
	if b != '[' && b != ']' || i+1 >= len(code) || code[i+1] != b {
		return false
	}
	return (i == 0 || isASCIISpace(code[i-1])) && (i+2 >= len(code) || isASCIISpace(code[i+2]))
}

// compoundRun is one rewrite_compound walk: the command, its tokens, the script depth and the output so far.
type compoundRun struct {
	r       *rewriter
	cmd     string
	toks    []token
	sw      int
	b       strings.Builder
	changed bool
}

func (r *rewriter) compound(cmd string, sw int) (string, bool) {
	c := &compoundRun{r: r, cmd: cmd, toks: tokenize(cmd), sw: sw}
	if opaquePipeline(c.toks) {
		return "", false
	}
	start := 0
	for _, t := range c.toks {
		if t.off < start || !isClauseEnd(t) && !t.isPipe() {
			continue
		}
		if start = c.clause(t, start); start < 0 {
			return c.b.String(), c.changed
		}
	}
	c.emitSeg(cmd[start:])
	return c.b.String(), c.changed
}

// clause writes the clause ending at t (or the pipeline t opens) and returns the next start, -1 when cmd is done.
func (c *compoundRun) clause(t token, start int) int {
	if !t.isPipe() {
		c.emitSeg(c.cmd[start:t.off])
		writeOp(&c.b, c.cmd, t)
		return skipSpaces(c.cmd, t.off+len(t.val))
	}
	return c.pipeline(start, t.off)
}

// opaquePipeline: a pipe next to ( ) { } grouping is left whole.
func opaquePipeline(toks []token) bool {
	return slices.ContainsFunc(toks, token.isPipe) && slices.ContainsFunc(toks, func(t token) bool {
		return t.kind == tkShellism && strings.Contains("(){}", t.val)
	})
}

// isClauseEnd is an operator (&& || ; ;; newline) or a background &.
func isClauseEnd(t token) bool { return t.kind == tkOp || t.kind == tkShellism && t.val == "&" }

func writeOp(b *strings.Builder, cmd string, t token) {
	switch {
	case t.kind == tkShellism:
		b.WriteString(" & ")
	case strings.HasPrefix(t.val, ";"):
		b.WriteString(t.val)
		if t.off+len(t.val) < len(cmd) {
			b.WriteByte(' ')
		}
	default:
		b.WriteString(" " + t.val + " ")
	}
}

func skipSpaces(cmd string, i int) int {
	for i < len(cmd) && cmd[i] == ' ' {
		i++
	}
	return i
}

// emitSeg writes seg's rewrite, or seg itself, and records a change.
func (c *compoundRun) emitSeg(seg string) {
	seg = strings.TrimSpace(seg)
	out, ok := c.r.segment(seg, ctxNormal, 0, c.sw)
	if !ok {
		out = seg
	}
	c.b.WriteString(out)
	c.changed = c.changed || out != seg
}

// pipeline writes the pipeline starting at start and returns where its next clause starts, -1 at the end.
func (c *compoundRun) pipeline(start, firstPipe int) int {
	p := analyzePipeline(c.cmd, c.toks, start, firstPipe)
	out, ok := c.pipeFinal(start, p)
	if !ok {
		out, ok = c.pipeProducer(start, firstPipe, p)
	}
	if !ok {
		out = strings.TrimSpace(c.cmd[start:p.end])
	}
	c.b.WriteString(out)
	c.changed = c.changed || ok
	return p.next
}

// pipeInfo is a pipeline's end, next clause and final stage (-1 when absent), and whether every consumer is safe.
type pipeInfo struct {
	end, next, final int
	allSafe          bool
}

// stageWalk is analyze_pipeline's running state over the pipe tokens.
type stageWalk struct {
	start, final     int
	structured, safe bool
}

func analyzePipeline(cmd string, toks []token, segStart, firstPipe int) pipeInfo {
	next, end := -1, len(cmd)
	if i := slices.IndexFunc(toks, func(t token) bool { return t.off > firstPipe && isClauseEnd(t) }); i >= 0 {
		next, end = toks[i].off, toks[i].off
	}
	w := stageWalk{start: segStart, final: -1, structured: true, safe: true}
	for i, t := range toks {
		if t.off >= firstPipe && t.off < end {
			w.visit(cmd, toks, i, firstPipe)
		}
	}
	w.finish(cmd, end)
	p := pipeInfo{end: end, next: next, final: -1, allSafe: w.structured && w.safe}
	if w.structured {
		p.final = w.final
	}
	return p
}

// finish judges the last stage: empty breaks the structure, an unsafe consumer the producer rewrite.
func (w *stageWalk) finish(cmd string, end int) {
	last := strings.TrimSpace(cmd[w.start:end])
	if last == "" {
		w.structured = false
	} else if !isSafeConsumer(last) {
		w.safe = false
	}
}

func (w *stageWalk) visit(cmd string, toks []token, i, firstPipe int) {
	t := toks[i]
	if t.kind == tkRedirect {
		w.safe = w.safe && !redirectHasFileTarget(toks, i)
		return
	}
	if !t.isPipe() {
		return
	}
	stage := strings.TrimSpace(cmd[w.start:t.off])
	w.structured = w.structured && stage != "" && t.kind != tkPipeErr
	w.safe = w.safe && (t.off <= firstPipe || isSafeConsumer(stage))
	w.start = t.off + len(t.val)
	w.final = w.start
}

// stage rewrites one pipeline stage in ctx; ok only when the rewrite changed it.
func (c *compoundRun) stage(s string, ctx rctx) (string, bool) {
	s = strings.TrimSpace(s)
	out, ok := c.r.segment(s, ctx, 0, c.sw)
	return out, ok && out != s
}

func (c *compoundRun) pipeFinal(start int, p pipeInfo) (string, bool) {
	if p.final < 0 {
		return "", false
	}
	out, ok := c.stage(c.cmd[p.final:p.end], ctxFinal)
	return strings.TrimSpace(c.cmd[start:p.final]) + " " + out, ok
}

func (c *compoundRun) pipeProducer(start, firstPipe int, p pipeInfo) (string, bool) {
	if !p.allSafe {
		return "", false
	}
	out, ok := c.stage(c.cmd[start:firstPipe], ctxProducer)
	return out + " " + strings.TrimSpace(c.cmd[firstPipe:p.end]), ok
}

func isSafeConsumer(stage string) bool {
	words := shellSplit(stage)
	if len(words) == 0 {
		return false
	}
	i := slices.IndexFunc(safeConsumers, func(c safeConsumer) bool { return c.name == words[0] })
	return i >= 0 && !slices.ContainsFunc(words[1:], func(arg string) bool { return argUnsafe(safeConsumers[i], arg) })
}

// argUnsafe: a long flag matches by any prefix abbreviation (--fol), a short cluster by any of its letters.
func argUnsafe(c safeConsumer, arg string) bool {
	if rest, ok := strings.CutPrefix(arg, "--"); ok {
		name, _, _ := strings.Cut(rest, "=")
		return name != "" && hasAny(name, c.unsafeLong, func(n, flag string) bool { return strings.HasPrefix(flag[2:], n) })
	}
	rest, ok := strings.CutPrefix(arg, "-")
	return ok && strings.ContainsAny(rest, c.unsafeChar)
}

func (r *rewriter) shellWrapper(cmd string, sw int) (string, bool) {
	w, ok := parseShellWrapper(cmd)
	if !ok {
		return "", false
	}
	script := cmd[w.start:w.end]
	if strings.ContainsAny(script, "\n\r") || hasHeredoc(script) || strings.Contains(script, "$((") ||
		unattestable(script, w.dialect) {
		return "", false
	}
	out, ok := r.compound(script, sw+1)
	return cmd[:w.start] + out + cmd[w.end:], ok
}

func (r *rewriter) segment(seg string, ctx rctx, depth, sw int) (string, bool) {
	t := strings.TrimSpace(seg)
	if t == "" {
		return "", false
	}
	if sw < 1 {
		if out, ok := r.shellWrapper(t, sw); ok {
			return out, true
		}
	}
	if depth >= maxPrefixDepth {
		return "", false
	}
	if out, ok, done := r.peel(t, ctx, depth, sw); done {
		return out, ok
	}
	return r.command(t, ctx)
}

// peel looks through an env prefix, a built-in transparent prefix or a process wrapper; done when one decided.
func (r *rewriter) peel(t string, ctx rctx, depth, sw int) (out string, ok, done bool) {
	if prefix, rest := stripEnvPrefix(t); prefix != "" {
		if strings.Contains(prefix, "RTK_DISABLED=") {
			return "", false, true
		}
		out, ok = r.segment(rest, ctx, depth+1, sw)
		return prefix + out, ok, true
	}
	if out, ok, done = r.peelTransparent(t, ctx, depth, sw); done {
		return out, ok, true
	}
	if prefix, rest, found := stripProcessWrapper(t); found {
		out, ok = r.segment(rest, ctx, depth+1, sw)
		return prefix + " " + out, ok, true
	}
	return "", false, false
}

func (r *rewriter) peelTransparent(t string, ctx rctx, depth, sw int) (out string, ok, done bool) {
	for _, p := range transparentPrefixes {
		rest, found := stripWordPrefix(t, p.word)
		if !found {
			continue
		}
		if rest == "" {
			return "", false, true
		}
		if out, ok = r.segment(rest, ctx, depth+1, sw); ok {
			return p.word + " " + out, true, true
		}
		_, inner := stripEnvPrefix(rest)
		return "", false, !p.routable || r.excluded(inner) // a routable wrapper falls through unless inner is excluded
	}
	return "", false, false
}

// stripEnvPrefix splits leading `env ` and VAR=value words off t; rest is trimmed.
func stripEnvPrefix(t string) (prefix, rest string) {
	t = strings.TrimSpace(t)
	end := 0
	if loc := prefixRx()["env"].FindStringIndex(t); loc != nil {
		end = loc[1]
	}
	return t[:end], strings.TrimSpace(t[end:])
}

func stripWordPrefix(cmd, prefix string) (string, bool) {
	if cmd == prefix {
		return "", true
	}
	if len(cmd) > len(prefix) && strings.HasPrefix(cmd, prefix) && cmd[len(prefix)] == ' ' {
		return strings.TrimLeftFunc(cmd[len(prefix)+1:], unicode.IsSpace), true
	}
	return "", false
}

// excluded is rtk's is_excluded for `^<literal>($|\s)` patterns, the only kind configExcludes accepts.
func (r *rewriter) excluded(cmd string) bool {
	for _, p := range r.excl {
		if rest, ok := strings.CutPrefix(cmd, p); ok && (rest == "" || startsSpace(rest)) {
			return true
		}
	}
	return false
}

// command decides a segment with no prefix left to peel (rewrite_segment_inner's tail).
func (r *rewriter) command(t string, ctx rctx) (string, bool) {
	part, redir := stripTrailingRedirects(t)
	if strings.HasPrefix(part, "rtk ") || part == "rtk" {
		return t, true
	}
	if ctx == ctxNormal && (strings.HasPrefix(part, "head ") || strings.HasPrefix(part, "tail ")) {
		out, ok := r.lineRange(part)
		return joinRedirect(out, redir), ok && !r.excluded(part)
	}
	if catFlagged(part) {
		return "", false
	}
	switch cls, idx := r.classify(part); cls {
	case clsUnsupported:
		return r.tomlFilter(part, redir, ctx)
	case clsSupported:
		return r.supported(part, redir, idx, ctx)
	}
	return "", false
}

// catFlagged: only `cat -n` maps onto rtk read; any other cat flag stays native.
func catFlagged(part string) bool {
	args, ok := strings.CutPrefix(part, "cat ")
	a := strings.TrimLeftFunc(args, unicode.IsSpace)
	return ok && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "-n ") && !strings.HasPrefix(a, "-n\t")
}

func startsSpace(s string) bool {
	c, _ := utf8.DecodeRuneInString(s)
	return s != "" && unicode.IsSpace(c)
}

// stripTrailingRedirects splits trailing redirects (and their targets) off cmd, keeping their exact text.
func stripTrailingRedirects(cmd string) (string, string) {
	toks := tokenize(cmd)
	bound := len(toks)
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].kind == tkRedirect {
			bound = i
			continue
		}
		if toks[i].kind != tkArg || i == 0 || toks[i-1].kind != tkRedirect {
			break
		}
		bound = i - 1
		i--
	}
	if bound >= len(toks) {
		return cmd, ""
	}
	part := strings.TrimRightFunc(cmd[:toks[bound].off], unicode.IsSpace)
	return part, cmd[len(part):]
}

func joinRedirect(out, redir string) string {
	if redir == "" || startsSpace(redir) {
		return out + redir
	}
	return out + " " + redir
}

func (r *rewriter) lineRange(cmd string) (string, bool) {
	px := prefixRx()
	if m := px["headRange"].FindStringSubmatch(cmd); m != nil {
		return r.readForm(m[5], firstNonEmpty(m[1:5]), "--head-lines")
	}
	if m := px["headBare"].FindStringSubmatch(cmd); m != nil {
		if out, ok := r.readForm(m[1], "10", "--head-lines"); ok {
			return out, true
		}
	}
	if strings.HasPrefix(cmd, "head") {
		return "", false
	}
	if m := px["tailRange"].FindStringSubmatch(cmd); m != nil {
		return r.readForm(m[5], firstNonEmpty(m[1:5]), "--tail-lines")
	}
	return "", false
}

func firstNonEmpty(list []string) string {
	for _, s := range list {
		if s != "" {
			return s
		}
	}
	return ""
}

// readForm is `rtk read FILE FLAG N` when file is one concrete operand; non-ASCII is left to the binary.
func (r *rewriter) readForm(file, n, flag string) (string, bool) {
	if file == "" || file[0] == '-' {
		return "", false
	}
	for i := range len(file) {
		c := file[i]
		if c >= 0x80 {
			r.unsure = true
			return "", false
		}
		if !isAlpha(c) && !isDigit(c) && strings.IndexByte("._/-~+@:,=", c) < 0 {
			return "", false
		}
	}
	return "rtk read " + file + " " + flag + " " + n, true
}

func (r *rewriter) supported(part, redir string, idx int, ctx rctx) (string, bool) {
	rule := firstRuleNamed(rtkRules[idx].name)
	_, clean := stripEnvPrefix(part)
	if len(r.excl) > 0 && (r.excluded(clean) || r.excluded(toolForm(clean, rule))) {
		return "", false
	}
	if !r.pipeAllowed(rule, part, ctx) {
		return "", false
	}
	out, ok := commandPart(rule, part)
	return out + redir, ok
}

// pipeAllowed is the rule's pipeline safety for ctx (rtk #3171).
func (r *rewriter) pipeAllowed(rule rtkRule, part string, ctx rctx) bool {
	switch ctx {
	case ctxFinal:
		return rule.safety == pipeBoth && patternFileFree(rule, part)
	case ctxProducer:
		return rule.safety != pipeNone && patternFileFree(rule, part) && r.lineFaithful(rule, part)
	}
	return true
}

func firstRuleNamed(name string) rtkRule {
	for _, r := range rtkRules {
		if r.name == name {
			return r
		}
	}
	return rtkRule{}
}

// patternFileFree: grep/rg reading patterns from a file (-f, --file) cannot take pipe input through rtk.
func patternFileFree(rule rtkRule, cmd string) bool {
	if rule.name != "rtk grep" && rule.name != "rtk rg" {
		return true
	}
	for _, arg := range shellSplit(cmd)[1:] {
		if arg == "--" {
			break
		}
		flags, short := strings.CutPrefix(arg, "-")
		if arg == "--file" || strings.HasPrefix(arg, "--file=") || short && !strings.HasPrefix(flags, "-") && strings.Contains(flags, "f") {
			return false
		}
	}
	return true
}

// lineFaithful approximates rtk's is_bare_file_list (not in the port's source): any list-shaped flag is unsure.
func (r *rewriter) lineFaithful(rule rtkRule, cmd string) bool {
	if rule.name != "rtk grep" && rule.name != "rtk rg" {
		return true
	}
	for _, arg := range shellSplit(cmd)[1:] {
		if arg == "--" {
			break
		}
		long, isLong := strings.CutPrefix(arg, "--")
		short, isShort := strings.CutPrefix(arg, "-")
		if isLong && strings.HasPrefix(long, "files") || !isLong && isShort && strings.ContainsAny(short, "lL") {
			r.unsure = true
		}
	}
	return true
}

func commandPart(rule rtkRule, part string) (string, bool) {
	if rule.name == "rtk golangci-lint run" {
		if global, run, ok := golangciParts(part); ok {
			return joinWords("rtk golangci-lint", global, run), true
		}
	}
	if lower := strings.ToLower(part); rule.name == "rtk gh" && (strings.Contains(lower, "--json") ||
		strings.Contains(lower, "--jq") || strings.Contains(lower, "--template")) {
		return "", false // gh structured output must stay raw
	}
	for _, p := range rule.prefixes {
		if rest, ok := stripWordPrefix(part, p); ok {
			return joinWords(rule.name, rest), true
		}
	}
	return "", false
}

// joinWords joins the non-empty parts with one space.
func joinWords(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " ")
}

// toolForm is the command spelled as exclude_commands names its tool: runner peeled, arguments kept.
func toolForm(clean string, rule rtkRule) string {
	n := stripAbsolutePath(clean)
	for _, p := range rule.prefixes {
		if rest, ok := stripWordPrefix(n, p); ok {
			return joinWords(toolPortion(p, rule), rest)
		}
	}
	return n
}

func toolPortion(prefix string, rule rtkRule) string {
	best, rest := prefix, prefix
	for {
		i := strings.IndexByte(rest, ' ')
		if i < 0 {
			return best
		}
		if rest = rest[i+1:]; slices.Contains(rule.prefixes, rest) {
			best = rest
		}
	}
}

func (r *rewriter) tomlFilter(part, redir string, ctx rctx) (string, bool) {
	if ctx != ctxNormal {
		return "", false
	}
	n := stripAbsolutePath(strings.TrimSpace(part))
	f := strings.Fields(n)
	if r.excluded(n) || len(f) == 0 || filterReserved[f[0]] {
		return "", false
	}
	folded := r.fold(n)
	for _, re := range filterRx() {
		if re.MatchString(folded) {
			return "rtk " + part + redir, true
		}
	}
	return "", false
}

// stripAbsolutePath turns `/usr/bin/grep -rn x` into `grep -rn x` (rtk #485).
func stripAbsolutePath(cmd string) string {
	end := strings.IndexByte(cmd, ' ')
	if end < 0 {
		end = len(cmd)
	}
	first := cmd[:end]
	if !strings.Contains(first, "/") || strings.HasSuffix(first, "/") {
		return cmd
	}
	return first[strings.LastIndexByte(first, '/')+1:] + cmd[end:]
}

// stripProcessWrapper splits timeout/time/nice/nohup and their options off cmd.
func stripProcessWrapper(cmd string) (prefix, rest string, ok bool) {
	toks := tokenize(cmd)
	if len(toks) == 0 || toks[0].kind != tkArg {
		return "", "", false
	}
	w, found := findWrapper(toks[0].val[strings.LastIndexByte(toks[0].val, '/')+1:])
	if !found {
		return "", "", false
	}
	idx, found := wrapperInner(w, toks)
	if !found || slices.ContainsFunc(toks[:idx], func(t token) bool { return t.val == "rtk" }) {
		return "", "", false
	}
	prefix = strings.TrimRightFunc(cmd[:toks[idx].off], unicode.IsSpace)
	rest = strings.TrimLeftFunc(cmd[toks[idx].off:], unicode.IsSpace)
	return prefix, rest, prefix != "" && rest != ""
}

func findWrapper(name string) (processWrapper, bool) {
	for _, w := range processWrappers {
		if w.name == name {
			return w, true
		}
	}
	return processWrapper{}, false
}

// wrapperInner is the index of the token the wrapper runs, past its options and positionals.
func wrapperInner(w processWrapper, toks []token) (int, bool) {
	idx, done, pos := 1, false, w.positionals
	for idx < len(toks) && toks[idx].kind == tkArg {
		arg := toks[idx].val
		if !done && arg == "--" {
			done = true
			idx++
			continue
		}
		if !done && isOption(w, arg) {
			n := optWidth(w, toks, idx)
			if n == 0 {
				return 0, false
			}
			idx += n
			continue
		}
		if pos == 0 {
			return idx, true
		}
		pos--
		idx++
	}
	return 0, false
}

func isOption(w processWrapper, arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-" || w.numeric && numericOpt(arg)
}

func optWidth(w processWrapper, toks []token, idx int) int {
	arg := toks[idx].val
	switch {
	case w.numeric && numericOpt(arg), slices.Contains(w.flagOpts, arg), attachedValue(w, arg):
		return 1
	case slices.Contains(w.valueOpts, arg) && idx+1 < len(toks) && toks[idx+1].kind == tkArg:
		return 2
	}
	return 0
}

func numericOpt(arg string) bool {
	d := strings.TrimPrefix(arg, "-")
	if d == arg {
		d = strings.TrimPrefix(arg, "+")
	}
	return d != arg && d != "" && strings.Trim(d, "0123456789") == ""
}

func attachedValue(w processWrapper, arg string) bool {
	if name, _, ok := strings.Cut(arg, "="); ok {
		return slices.Contains(w.valueOpts, name)
	}
	return hasAny(arg, w.attached, func(a, opt string) bool { return len(a) > len(opt) && strings.HasPrefix(a, opt) })
}
