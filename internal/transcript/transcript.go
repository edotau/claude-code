// Package transcript reads Claude Code session transcript JSONL: the live context size and the files a session wrote.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/models"
)

// tailBytes bounds the common-case read: the latest usage record sits near EOF of an append-only file.
const tailBytes = 256 << 10

// Usage is the newest assistant usage record: total context tokens and the model that produced it.
type Usage struct {
	Tokens int
	Model  string
}

// LastUsage scans the tail back-to-front, falling back to a full scan when the tail holds no usage line.
func LastUsage(path string) (Usage, bool) {
	f, err := os.Open(path)
	if err != nil || path == "" {
		return Usage{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Usage{}, false
	}
	off := max(fi.Size()-tailBytes, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return Usage{}, false
	}
	for end := len(buf); end > 0; {
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		if u, ok := usageFromLine(buf[start:end]); ok {
			return u, true
		}
		end = start - 1
	}
	if off == 0 {
		return Usage{}, false
	}
	_, _ = f.Seek(0, io.SeekStart)
	var last Usage
	found := false
	EachLine(f, func(line []byte) {
		if u, ok := usageFromLine(line); ok {
			last, found = u, true
		}
	})
	return last, found
}

var usageKey = []byte(`"usage"`)

func usageFromLine(line []byte) (Usage, bool) {
	if !bytes.Contains(line, usageKey) {
		return Usage{}, false
	}
	var rec struct {
		IsSidechain bool `json:"isSidechain"`
		Message     struct {
			Model string `json:"model"`
			Usage *struct {
				Input         int `json:"input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
				Output        int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil || rec.Message.Usage == nil || rec.IsSidechain {
		return Usage{}, false
	}
	u := rec.Message.Usage
	return Usage{Tokens: u.Input + u.CacheRead + u.CacheCreation + u.Output, Model: rec.Message.Model}, true
}

// Window is the context window for a session model: 1M when usage already exceeds 200K or the launch
// env pins a [1m] variant of the model (transcripts record the wire id, never the marker).
func Window(model string, used int) int {
	if used > models.ContextWindowStd {
		return models.ContextWindow1M
	}
	if strings.HasSuffix(model, "[1m]") {
		return models.ContextWindow1M
	}
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		v := os.Getenv(k)
		if base := models.Strip1M(v); model != "" && (base == model || strings.HasSuffix(base, ":"+model)) {
			return models.ContextWindow(v)
		}
	}
	return models.ContextWindow(model)
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
var toolUseKey = []byte(`"tool_use"`)
var commitKey = []byte("commit") // cheap prefilter; the real check is gitCommitCommand (segment/token aware)
var bashNameKey = []byte(`"name":"Bash"`)

// editNameKeys are the literal `"name":"<Tool>"` byte patterns for editTools — cheap enough to rule a
// line out before paying for json.Unmarshal, since every other tool_use (Read/Grep/Bash…) fails all four.
var editNameKeys = [][]byte{
	[]byte(`"name":"Edit"`), []byte(`"name":"Write"`), []byte(`"name":"MultiEdit"`), []byte(`"name":"NotebookEdit"`),
}

func hasEditToolName(line []byte) bool {
	for _, k := range editNameKeys {
		if bytes.Contains(line, k) {
			return true
		}
	}
	return false
}

// CommitEditWeight is how many edits a Bash `git commit` call counts for: one commit alone is
// substantive work; hooks.harvestMinEdits is defined from this so the two stay in lockstep.
const CommitEditWeight = 3

// EditedFiles returns up to limit unique paths the transcript's Edit/Write tool calls touched, sorted.
func EditedFiles(path string, limit int) []string {
	seen := map[string]bool{}
	eachEdit(path, func(p string) {
		if p != "" {
			seen[p] = true
		}
	})
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// CountEdits counts editor tool_use calls (plus weighted commits) in the transcript's complete JSONL records.
func CountEdits(path string) int {
	n, _ := CountEditsFrom(path, 0)
	return n
}

// CountEditsFrom counts complete lines from offset and returns the position after the last newline.
func CountEditsFrom(path string, offset int64) (edits int, next int64) {
	offset = max(offset, 0)
	f, err := os.Open(path)
	if err != nil {
		return 0, offset
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return 0, offset
	}
	if fi.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, offset
	}
	n := 0
	read := scanLines(f, true, func(line []byte) {
		n += countLineEdits(line)
	})
	return n, offset + read
}

// countLineEdits decodes an assistant tool_use line at most once, counting editor calls and weighted
// git commits — eachEditLine and eachCommitLine used to each re-unmarshal the same line.
func countLineEdits(line []byte) (n int) {
	defer func() { _ = recover() }() // malformed command text must not crash the hook; keep edits counted so far
	if !bytes.Contains(line, toolUseKey) {
		return 0
	}
	isEdit := hasEditToolName(line)
	isBash := bytes.Contains(line, bashNameKey) && bytes.Contains(line, commitKey)
	if !isEdit && !isBash {
		return 0
	}
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					Command string `json:"command"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" {
		return 0
	}
	for _, c := range rec.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		switch {
		case editTools[c.Name]:
			n++
		case c.Name == "Bash" && gitCommitCommand(c.Input.Command):
			n += CommitEditWeight
		}
	}
	return n
}

// eachEdit calls fn with the target path of each editor tool_use in an assistant record.
func eachEdit(path string, fn func(filePath string)) {
	f, err := os.Open(path)
	if err != nil || path == "" {
		return
	}
	defer f.Close()
	EachLine(f, func(line []byte) {
		eachEditLine(line, fn)
	})
}

func eachEditLine(line []byte, fn func(filePath string)) {
	if !bytes.Contains(line, toolUseKey) || !hasEditToolName(line) {
		return
	}
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					FilePath     string `json:"file_path"`
					NotebookPath string `json:"notebook_path"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" {
		return
	}
	for _, c := range rec.Message.Content {
		if c.Type == "tool_use" && editTools[c.Name] {
			fn(c.Input.FilePath + c.Input.NotebookPath)
		}
	}
}

// gitValueOpts are git global options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--super-prefix": true, "--config-env": true,
}

// gitShortCircuitOpts are git global options that print something and exit before any verb runs —
// `--exec-path` only short-circuits bare (an `=`-valued form supplies a path, not a request to print one).
var gitShortCircuitOpts = map[string]bool{
	"--version": true, "--help": true, "-h": true, "--exec-path": true,
	"--html-path": true, "--man-path": true, "--info-path": true,
}

// maxShellRecursionDepth bounds `bash -c "..."` (and sh/zsh) recursion into the quoted argument.
const maxShellRecursionDepth = 3

// shellCNames are the interpreters whose `-c <script>` argument runs as a fresh shell command line.
var shellCNames = map[string]bool{"bash": true, "sh": true, "zsh": true}

// gitCommitCommand reports whether cmd actually RUNS `git … commit …` in some shell segment: split on
// &&/||/;/|/newline, tokenize each segment (quotes collapse to one token, so quoted text never matches),
// skip a leading env assignment or sudo/env, then require `git` (or a .../git path) followed by global
// options up to the verb `commit`. Heredoc bodies are skipped first (this is a counter, not the safety
// guard — undercounting a piped heredoc body is acceptable); `$(...)`/backtick substitutions are scanned
// too, since a captured `git commit` still runs.
func gitCommitCommand(cmd string) bool {
	return gitCommitCommandDepth(cmd, 0)
}

func gitCommitCommandDepth(cmd string, depth int) bool {
	cmd = stripHeredocBodies(cmd)
	if commitInText(cmd, depth) {
		return true
	}
	for _, sub := range extractSubstitutions(cmd) {
		if commitInText(stripHeredocBodies(sub), depth) {
			return true
		}
	}
	return false
}

// commitInText runs the segment/token git-commit check over one piece of shell text (a command, or a
// `$(...)`/backtick substitution's inner text); depth bounds `bash -c`-style recursion.
func commitInText(text string, depth int) bool {
	for _, seg := range commitShellSegments(text) {
		toks := skipCommitWrappers(commitShellTokens(seg))
		if len(toks) == 0 {
			continue
		}
		if commitViaShellC(toks, depth) || isGitCommitInvocation(toks, depth) {
			return true
		}
	}
	return false
}

// commitViaShellC reports whether toks is a `bash -c "..."`-style wrapper whose quoted script commits.
func commitViaShellC(toks []string, depth int) bool {
	if depth >= maxShellRecursionDepth || !shellCNames[shellBaseName(toks[0])] {
		return false
	}
	inner, ok := findDashCArg(toks[1:])
	return ok && gitCommitCommandDepth(inner, depth+1)
}

// isGitCommitInvocation reports whether toks is a `git [global opts] commit` invocation (alias or literal).
func isGitCommitInvocation(toks []string, depth int) bool {
	if toks[0] != "git" && !strings.HasSuffix(toks[0], "/git") {
		return false
	}
	i := 1
	shortCircuit := false
	aliases := map[string]string{}
	for i < len(toks) && strings.HasPrefix(toks[i], "-") {
		if gitShortCircuitOpts[toks[i]] {
			shortCircuit = true
			break
		}
		if gitValueOpts[toks[i]] && !strings.Contains(toks[i], "=") {
			if toks[i] == "-c" && i+1 < len(toks) {
				if name, val, ok := splitAliasAssign(toks[i+1]); ok && !gitBuiltinVerbs[name] {
					aliases[name] = val // last -c alias.<name>=<val> for a given name wins
				}
			}
			i += 2
			continue
		}
		i++
	}
	if shortCircuit {
		return false
	}
	return i < len(toks) && (toks[i] == "commit" || aliasIsCommit(aliases[toks[i]], depth))
}

// shellBaseName strips a leading directory from a `/bin/bash`-style interpreter path.
func shellBaseName(tok string) string {
	if _, after, ok := strings.CutLast(tok, "/"); ok {
		return after
	}
	return tok
}

// shellLeadingFlags are bash/sh/zsh flags with no attached value, skippable before -c.
var shellLeadingFlags = map[string]bool{"-e": true, "-x": true, "-l": true, "--norc": true, "--noprofile": true}

// findDashCArg returns the argument following a `-c` flag among a shell interpreter's leading flags —
// combined short opts ending in `c` (`-xc`, `-ec`, `-exc`) count as `-c`; an `-n`/no-exec flag anywhere
// in a combined form (including `-nc`) means the script never runs, so it reports no match at all.
func findDashCArg(toks []string) (string, bool) {
	i := 0
	for i < len(toks) {
		t := toks[i]
		if !strings.HasPrefix(t, "-") {
			return "", false
		}
		if t == "-c" {
			if i+1 < len(toks) {
				return toks[i+1], true
			}
			return "", false
		}
		if t == "-o" {
			i += 2
			continue
		}
		if !strings.HasPrefix(t, "--") {
			body := t[1:]
			if strings.ContainsRune(body, 'n') {
				return "", false // -n (noexec) blocks execution even combined with a trailing c
			}
			if strings.HasSuffix(body, "c") {
				if i+1 < len(toks) {
					return toks[i+1], true
				}
				return "", false
			}
		}
		if shellLeadingFlags[t] {
			i++
			continue
		}
		return "", false
	}
	return "", false
}

// gitBuiltinVerbs are git's own command names — git ignores a user alias defined under one of these
// names, so `-c alias.<builtin>=…` never registers (the builtin behavior still runs unaltered).
var gitBuiltinVerbs = map[string]bool{
	"status": true, "add": true, "log": true, "diff": true, "push": true, "pull": true, "fetch": true,
	"checkout": true, "switch": true, "branch": true, "merge": true, "rebase": true, "reset": true,
	"show": true, "tag": true, "stash": true, "clone": true, "init": true, "rm": true, "mv": true,
	"restore": true, "commit": true,
}

// splitAliasAssign splits `alias.<name>=<value>` (from `git -c <val>`) into name and raw value.
func splitAliasAssign(val string) (name, value string, ok bool) {
	n, v, hasEq := strings.Cut(val, "=")
	if !hasEq {
		return "", "", false
	}
	const prefix = "alias."
	if !strings.HasPrefix(n, prefix) {
		return "", "", false
	}
	return strings.TrimPrefix(n, prefix), v, true
}

// aliasIsCommit reports whether an alias's raw value runs a commit: either directly (`commit`) or as a
// shell alias (`!git commit …`, recursed depth-limited); val == "" means no alias was registered.
func aliasIsCommit(val string, depth int) bool {
	if val == "" {
		return false
	}
	if val == "commit" {
		return true
	}
	if rest, ok := strings.CutPrefix(val, "!"); ok {
		return depth < maxShellRecursionDepth && gitCommitCommandDepth(rest, depth+1)
	}
	return false
}

// extractSubstitutions returns the inner text of every `$(...)` and backtick command substitution in
// cmd, recursively — quoting around the substitution is irrelevant to whether its inner command runs.
func extractSubstitutions(cmd string) []string {
	var out []string
	for i := 0; i < len(cmd); i++ {
		switch {
		case cmd[i] == '`':
			j := strings.IndexByte(cmd[i+1:], '`')
			if j < 0 {
				return out
			}
			inner := cmd[i+1 : i+1+j]
			out = append(out, inner)
			out = append(out, extractSubstitutions(inner)...)
			i += 1 + j
		case cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '(' && (i+2 >= len(cmd) || cmd[i+2] != '('):
			depth, k := 1, i+2
			for k < len(cmd) && depth > 0 {
				switch cmd[k] {
				case '(':
					depth++
				case ')':
					depth--
				}
				k++
			}
			if depth > 0 {
				return out // unterminated `$(` — nothing closes it, so stop scanning rather than slice
			}
			inner := cmd[i+2 : k-1]
			out = append(out, inner)
			out = append(out, extractSubstitutions(inner)...)
			i = k - 1
		}
	}
	return out
}

// stripHeredocBodies removes the body lines of every `<<WORD`/`<<'WORD'`/`<<-WORD` heredoc (and its
// terminator line) from cmd, so a commit mentioned only inside a heredoc body never counts as run.
func stripHeredocBodies(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		word, ok := heredocWord(lines[i])
		if !ok {
			continue
		}
		i++
		for i < len(lines) && strings.TrimSpace(lines[i]) != word {
			i++
		}
		// i now sits on the terminator line (or past EOF); either way it is dropped, not re-appended.
	}
	return strings.Join(out, "\n")
}

// heredocWord reports the terminator word of a `<<[-]WORD` (quoted or bare) on line, false for a
// here-string (`<<<`) or a line with no heredoc redirect at all.
func heredocWord(line string) (string, bool) {
	idx := strings.Index(line, "<<")
	if idx < 0 || (idx+2 < len(line) && line[idx+2] == '<') {
		return "", false
	}
	rest := strings.TrimLeft(strings.TrimPrefix(line[idx+2:], "-"), " \t")
	if rest == "" {
		return "", false
	}
	if rest[0] == '\'' || rest[0] == '"' {
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return "", false
		}
		return rest[1 : 1+end], rest[1:1+end] != ""
	}
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		end = len(rest)
	}
	word := rest[:end]
	return word, word != ""
}

// commitWrapperCmds pass their argument list through to run as the real command, not git themselves.
var commitWrapperCmds = map[string]bool{"sudo": true, "env": true, "time": true, "command": true, "exec": true, "nohup": true}

// commitShellKeywords start a fresh command after a shell control-flow keyword (if/while/for bodies).
var commitShellKeywords = map[string]bool{"then": true, "do": true, "else": true, "elif": true}

// sudoValueFlags are sudo flags whose value is the next token (`sudo -u user …`, `sudo -g group …`).
var sudoValueFlags = map[string]bool{"-u": true, "--user": true, "-g": true, "--group": true}

// envValueFlags are env flags whose value is the next token (`env -u NAME …`); `--` also ends env's
// own flags (env's next token is the command even if it starts with `-`).
var envValueFlags = map[string]bool{"-u": true, "--unset": true}

// skipCommitWrappers drops leading `NAME=value` env assignments, a leading `!`/shell keyword, and
// wrapper commands (`sudo`, `env`, `time`, `command`, `exec`, `nohup` — plus sudo's/env's own flags)
// before `git` (`env -i PATH=/x git commit` must still reach `git`).
func skipCommitWrappers(toks []string) []string {
	for len(toks) > 0 {
		if toks[0] == "!" || commitShellKeywords[toks[0]] {
			toks = toks[1:]
			continue
		}
		if commitWrapperCmds[toks[0]] {
			wrapper := toks[0]
			toks = toks[1:]
			for wrapper == "sudo" && len(toks) > 0 && strings.HasPrefix(toks[0], "-") {
				if sudoValueFlags[toks[0]] {
					toks = toks[1:]
				}
				toks = toks[1:]
			}
			for wrapper == "env" && len(toks) > 0 && strings.HasPrefix(toks[0], "-") {
				if toks[0] == "--" {
					toks = toks[1:]
					break
				}
				if envValueFlags[toks[0]] {
					toks = toks[1:]
				}
				toks = toks[1:]
			}
			continue
		}
		if name, _, ok := strings.Cut(toks[0], "="); ok && name != "" && !strings.ContainsAny(name, " \t") {
			toks = toks[1:]
			continue
		}
		break
	}
	return toks
}

// commitShellSegments splits cmd on &&, ||, ;, |, newline — quoted delimiters stay inside their segment.
// A backslash escapes the next char literally outside single quotes (so `\"` never closes a double
// quote); a backslash-newline outside single quotes is a line continuation and vanishes entirely.
func commitShellSegments(cmd string) []string {
	var out []string
	var b strings.Builder
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote == '\'' {
			b.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\\' && i+1 < len(cmd) {
			if cmd[i+1] == '\n' {
				i++
				continue
			}
			b.WriteByte(c)
			b.WriteByte(cmd[i+1])
			i++
			continue
		}
		if quote == '"' {
			b.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			b.WriteByte(c)
		case ';', '|', '&', '\n':
			if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') {
				i++
			}
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	return append(out, b.String())
}

// dqEscapable are the chars a backslash actually escapes inside double quotes (bash rule); before any
// other char the backslash stays a literal character of the token (`"comm\it"` keeps its backslash).
const dqEscapable = "$`\"\\\n"

// commitShellTokens splits one segment into words on whitespace; a quoted run collapses to ONE token,
// so `grep 'git commit'` never tokenizes into separate "git"/"commit" words that could match. Outside
// quotes a backslash escapes the next char literally; inside double quotes only dqEscapable collapses.
func commitShellTokens(seg string) []string {
	var out []string
	var b strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if quote == '\'' {
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
			continue
		}
		if c == '\\' && i+1 < len(seg) && (quote != '"' || strings.IndexByte(dqEscapable, seg[i+1]) >= 0) {
			started = true
			b.WriteByte(seg[i+1])
			i++
			continue
		}
		if quote == '"' {
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			started = true
		case ' ', '\t':
			if started {
				out = append(out, b.String())
				b.Reset()
				started = false
			}
		default:
			started = true
			b.WriteByte(c)
		}
	}
	if started {
		out = append(out, b.String())
	}
	return out
}

// EachLine calls fn per line of r (oversized lines joined, partial EOF record included); slice valid only in fn.
func EachLine(r io.Reader, fn func([]byte)) {
	scanLines(r, false, fn)
}

// contentPart is one element of a message.content array; only {type:"text"} parts carry transcript text.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ContentText flattens message.content (bare string, or {type,text} parts) to its text parts joined; others skipped.
func ContentText(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []contentPart
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// readerPool reuses the 1MB bufio.Reader across calls — this runs on every Stop hook, so a fresh
// 1<<20 allocation per scan is wasted churn.
var readerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 1<<20) }}

// scanLines avoids common-case copies and joins oversized lines without stopping the scan.
func scanLines(r io.Reader, completeOnly bool, fn func([]byte)) int64 {
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(r)
	defer readerPool.Put(br)
	var long []byte
	var read int64
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, line...)
			continue
		}
		if long != nil {
			line, long = append(long, line...), nil
		}
		if len(line) > 0 && (!completeOnly || err == nil) {
			fn(line)
			read += int64(len(line))
		}
		if err != nil {
			return read
		}
	}
}
