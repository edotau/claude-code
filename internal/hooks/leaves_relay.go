package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
)

// relayBash refuses a relay's Bash call that is not a claude-code, read-only or go gate command: the job does the work.
func relayBash(in *core.Input) (core.Result, bool) {
	cmd := in.Nested("tool_input", "command")
	if _, relay := relayLeaf(in); !relay || cmd == "" {
		return core.Result{}, false
	}
	for _, seg := range shell.Segments(cmd) {
		if rule := relayRefusal(seg); rule != "" {
			shown := textutil.Truncate(strings.Join(strings.Fields(textutil.RedactSecrets(seg)), " "), 60)
			return core.Result{Decision: core.Block, Rule: "relay-shell", Verdict: core.VerdictRefused, Reason: fmt.Sprintf("relay: refused `%s`: %s; run only "+
				"`claude-code <agent> ask ... - < /absolute/brief/file` (no cd: use --workdir), read-only commands "+
				"(git diff, grep, ls, cat), or: "+relayGoShapes+".", shown, rule)}, true
		}
	}
	return core.Result{}, false
}

// relayRefusal names the rule a relay's Bash segment breaks; "" when it is allowed.
func relayRefusal(seg string) string {
	if slices.ContainsFunc(shell.Tokens(seg), relayGoEnvRe().MatchString) {
		return "sets a Go environment variable (GOFLAGS, GOENV, GOWORK, GOBIN, GOCACHE or GOTOOLCHAIN)"
	}
	if toks := relayWords(seg); len(toks) == 0 || filepath.Base(toks[0]) != "claude-code" {
		if agent.ReadOnlyShell(seg) {
			return ""
		}
		if rule, isGo := relayGoGate(seg); isGo {
			return rule
		}
		return "not a `claude-code` command"
	}
	if cc := agentCommand(relayWords(seg)); len(cc) > 1 && cc[0] == "worktree" && cc[1] == "commit" {
		return "a relay does not commit; the dispatched agent or the main session commits"
	}
	_, _, err := relayInput(seg)
	if err != nil {
		return err.Error()
	}
	return ""
}

// relayGoEnvFlags write files or run other programs; a GOFLAGS carrying one is refused (leading dashes dropped).
var relayGoEnvFlags = sync.OnceValue(func() []string { return shell.GuardWords("relay_go_env_flags") })

// relayGoEnvRe is an inline assignment of a Go variable that can redirect what the toolchain runs or writes.
var relayGoEnvRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(` + strings.Join(shell.GuardWords("relay_go_env_vars"), "|") + `)=`)
})

// relayGoValue and relayGoBool are the flags a relay's go gate allows, by subcommand; a value flag takes one word.
var relayGoValue = sync.OnceValue(func() map[string][]string { return shell.GuardTools("relay_go_value_flags") })

var relayGoBool = sync.OnceValue(func() map[string][]string { return shell.GuardTools("relay_go_bool_flags") })

// relayGoShapes names the allowed go shapes in one line for the refusal message.
const relayGoShapes = "go vet|test <./packages> [-tags -p -race -v], go test also [-run -skip -count -timeout -shuffle -list " +
	"-short -failfast -cover -covermode -coverpkg -bench -benchmem -benchtime -parallel -cpu -vet -json], " +
	"go build ./... or go build <./packages> -o /dev/null, gofmt -l <./paths>"

// relayWrapRe is one leading wrapper in a safe shape; env takes no flag, and an assignment after it fails the go check.
var relayWrapRe = regexp.MustCompile(`^(?:rtk|env|(?:command )?time(?: -p)?|nice(?: -n -?[0-9]+)?|` +
	`g?timeout(?: -k [0-9]+[smh]?| --kill-after=[0-9]+[smh]?| -s [A-Za-z0-9]+| --signal=[A-Za-z0-9]+)*` +
	` [0-9]+[smh]?) `)

// relayUnwrap drops the leading wrappers relayWrapRe accepts, so the rest meets the checks of a bare command.
func relayUnwrap(toks []string) []string {
	for len(toks) > 1 {
		m := relayWrapRe.FindString(strings.Join(toks, " "))
		if m == "" {
			break
		}
		toks = toks[strings.Count(m, " "):]
	}
	return toks
}

// relayGoGate judges `go build|vet|test`, `gofmt -l` and their wrappers: !isGo for others, else rule is "" or why not.
func relayGoGate(seg string) (rule string, isGo bool) {
	toks := relayUnwrap(shell.Tokens(seg))
	if len(toks) < 2 || strings.ContainsAny(seg, "<>") || relayExpands(seg) {
		return "", false
	}
	if toks[0] == "gofmt" {
		return relayGofmtArgs(toks[1:]), true
	}
	if _, gated := relayGoValue()[toks[1]]; toks[0] != "go" || !gated { // the gated verbs are the value-flag table's
		return "", false
	}
	if rule := relayGoEnvRule(); rule != "" {
		return rule, true
	}
	return relayGoArgs(toks[1], toks[2:]), true
}

// relayExpands reports a backtick, `$(`, `${` or `$NAME` outside single quotes; a trailing `$` is literal.
func relayExpands(seg string) bool { return shell.Expands(seg) }

// relayGofmtArgs is "" for `gofmt -l` with relative paths inside the repository only.
func relayGofmtArgs(args []string) string {
	const bad = "gofmt is allowed only as `gofmt -l <relative paths inside the repository>`"
	if !slices.Contains(args, "-l") || len(args) < 2 {
		return bad
	}
	for _, a := range args {
		if a != "-l" && (strings.HasPrefix(a, "-") || !relayPathOK(a)) {
			return bad
		}
	}
	return ""
}

// relayPathOK is true for a relative path with no `..` element and no `~` or flag lead.
func relayPathOK(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "-") {
		return false
	}
	return !slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..")
}

// relayPkgOK is true for a `./` package pattern inside the repository.
func relayPkgOK(p string) bool { return strings.HasPrefix(p, "./") && relayPathOK(p) }

// relayGoEnvRule refuses a GOFLAGS this process sees with a refused flag; the relay shell's env is invisible.
func relayGoEnvRule() string {
	for f := range strings.FieldsSeq(os.Getenv("GOFLAGS")) {
		name, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if slices.Contains(relayGoEnvFlags(), name) {
			return "the inherited GOFLAGS carries -" + name
		}
	}
	return ""
}

// relayGoArgs is "" when args are in-repo package patterns and allowed flags only, and a build cannot drop a binary.
func relayGoArgs(sub string, args []string) string {
	var pkgs []string
	devNull := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if !relayPkgOK(a) {
				return "package " + a + " must be a ./ pattern inside the repository"
			}
			pkgs = append(pkgs, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if slices.Contains(relayGoBool()[sub], name) {
			continue
		}
		if !slices.Contains(relayGoValue()[sub], name) {
			return "flag " + a + " is not allowed (or it writes files or runs another program)"
		}
		if !hasVal && i+1 < len(args) {
			i++
			val = args[i]
		} else if !hasVal {
			return "flag " + a + " needs a value"
		}
		if rule := relayFlagValue(sub, name, a, val); rule != "" {
			return rule
		}
		devNull = devNull || name == "o"
	}
	if sub == "build" && !devNull && !slices.Equal(pkgs, []string{"./..."}) {
		return "go build is allowed only as `go build ./...` or with `-o /dev/null`"
	}
	return ""
}

// relayFlagValue judges one flag's value: no leading dash, `-o` only /dev/null, `-coverpkg` in-repo patterns.
func relayFlagValue(sub, name, flag, val string) string {
	switch {
	case strings.HasPrefix(val, "-"):
		return "flag " + flag + " needs a value that does not begin with -"
	case sub == "build" && name == "o" && val != "/dev/null":
		return "go build -o is allowed only as `-o /dev/null`"
	case name == "coverpkg" && !slices.ContainsFunc(strings.Split(val, ","), func(p string) bool { return !relayPkgOK(p) }):
		return ""
	case name == "coverpkg":
		return "-coverpkg must list ./ package patterns inside the repository"
	}
	return ""
}

// relayWords shares wrapper handling without changing the safety scanner's Unwrap behavior.
func relayWords(seg string) []string {
	words := shell.Tokens(seg)
	for {
		words, _ = shell.Unwrap(words, "/")
		if len(words) == 0 || filepath.Base(words[0]) != "rtk" {
			return words
		}
		words = words[1:]
	}
}

func relayCommands(cmd string) [][]string {
	var out [][]string
	for _, seg := range shell.Segments(cmd) {
		out = append(out, relayWords(seg))
	}
	return out
}

func agentCommand(words []string) []string {
	if len(words) < 3 || filepath.Base(words[0]) != "claude-code" {
		return nil
	}
	words = words[1:]
	if len(words) > 2 && words[0] == "--remote" {
		words = words[2:]
	}
	return words
}

func askVerb(words []string) string {
	words = agentCommand(words)
	if len(words) < 2 {
		return ""
	}
	if words[0] == "agents" && slices.Contains([]string{"ask", "dispatch", "result"}, words[1]) {
		return words[1]
	}
	if externalTypes[words[0]] && words[1] == "ask" {
		return "ask"
	}
	return ""
}

// reviewVerb is `claude-code <external agent> review`: a vendor run that is no ask, so no job id is waited on.
func reviewVerb(words []string) bool {
	words = agentCommand(words)
	return len(words) > 1 && externalTypes[words[0]] && words[1] == "review"
}

// relayInput permits only literal argv and one stdin file redirect; no expansions or shell execution.
func relayInput(seg string) (words []string, input string, err error) {
	if !relayQuotesClosed(seg) {
		return nil, "", errors.New("has an unclosed quote or escape")
	}
	at := shell.QuoteAt(seg)
	redirect := -1
	for i, c := range []byte(seg) {
		if at[i] == '\'' {
			continue
		}
		if c == '`' || c == '$' {
			return nil, "", errors.New("contains `$` or a backtick")
		}
		if at[i] == 0 && strings.ContainsRune(";|&(){}>*?[]~#", rune(c)) {
			return nil, "", fmt.Errorf("contains an unquoted shell operator `%c`", c)
		}
		if c == '<' && at[i] == 0 {
			if redirect >= 0 || i > 0 && !strings.ContainsRune(" \t", rune(seg[i-1])) {
				return nil, "", errors.New("redirect is not a literal `< file`")
			}
			redirect = i
		}
	}
	if redirect < 0 {
		return shell.Tokens(seg), "", nil
	}
	files := shell.Tokens(seg[redirect+1:])
	if len(files) != 1 || files[0] == "" {
		return nil, "", errors.New("redirect is not a literal `< file`")
	}
	return shell.Tokens(seg[:redirect]), files[0], nil
}

// askCmdRe retains the opening-line match API while checking command position with the shared parser.
func askCmdRe() askMatcher { return askMatcher{} }

type askMatcher struct{}

func (askMatcher) MatchString(cmd string) bool {
	for _, words := range relayCommands(cmd) {
		if askVerb(words) != "" {
			return true
		}
	}
	return false
}

func (askMatcher) FindStringSubmatchIndex(cmd string) []int {
	offset := 0
	for _, seg := range shell.Segments(cmd) {
		words := relayWords(seg)
		if askVerb(words) != "" {
			all := shell.Tokens(seg)
			skip := len(all) - len(words)
			spans := shellWordRe().FindAllStringIndex(seg, -1)
			if skip >= 0 && skip < len(spans) {
				return []int{offset, offset + len(seg), offset + spans[skip][0], offset + len(seg)}
			}
		}
		offset += len(seg) + 1
	}
	return nil
}

func relayQuotesClosed(seg string) bool {
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c == '\\' && quote != '\'' {
			i++
			if i == len(seg) {
				return false
			}
			continue
		}
		if c == quote {
			quote = 0
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
		}
	}
	return quote == 0
}
