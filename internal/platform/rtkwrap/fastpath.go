package rtkwrap

import (
	"bufio"
	"bytes"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
)

// FastPath is `rtk rewrite`'s answer for cmd sans exec when certain: "" runs cmd as typed, "rtk "+cmd wraps it.
func FastPath(cmd string) (rewritten string, decided bool) {
	if !MayRewrite(cmd) {
		return "", true
	}
	if os.Getenv(paths.RTKBinaryEnv) != "" { // a stand-in rtk (a wrapper, a test fake) answers for itself
		return "", false
	}
	if out, ok := simpleRewrite(cmd); ok {
		return out, true
	}
	return "", noSegmentRewrites(cmd)
}

// extraRewriteWords are heads `rtk rewrite` wraps that its --help (and so rtk_rewrite_words) does not list.
var extraRewriteWords = []string{"ansible-playbook", "biome", "hadolint", "markdownlint", "mise", "nx", "shellcheck",
	"turbo", "yamllint"}

// rewriteWords are the words rtk rewrite may act on: rtk_rewrite_words plus extraRewriteWords.
var rewriteWords = sync.OnceValue(func() map[string]bool {
	words := shell.GuardSet("rtk_rewrite_words")
	for _, w := range extraRewriteWords {
		words[w] = true
	}
	return words
})

// wordRun is one word of a command, and versioned a versioned spelling of one (gcc-13, python3.12).
var wordRun = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[\w.+-]+`) })
var versioned = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[-.]?[0-9][0-9.]*$`) })

// MayRewrite is false only when no word of cmd is one rtk rewrites, so skipping the exec loses nothing.
func MayRewrite(cmd string) bool {
	return slices.ContainsFunc(wordRun().FindAllString(cmd, -1), isRewriteWord)
}

func isRewriteWord(w string) bool {
	return rewriteWords()[w] || rewriteWords()[versioned().ReplaceAllString(w, "")]
}

// simpleRule is a head rtk wraps whole: at least minArgs arguments and, when subs is set, one of them first.
type simpleRule struct {
	minArgs int
	subs    []string
}

// simpleRules are the heads whose plain one-segment command rtk rewrite turns into "rtk "+cmd (rtk 1.1.0, verified).
var simpleRules = map[string]simpleRule{
	"ls": {}, "tree": {}, "wc": {}, "du": {}, "df": {}, "ps": {}, "stat": {}, "jq": {}, "make": {},
	"find": {minArgs: 1}, "grep": {minArgs: 1}, "rg": {minArgs: 1}, "curl": {minArgs: 1}, "diff": {minArgs: 1},
	"git":           {subs: []string{"status", "diff", "log", "branch", "show", "add", "push", "pull", "fetch", "stash"}},
	"go":            {subs: []string{"test", "build"}},
	"kubectl":       {subs: []string{"get", "logs", "describe"}},
	"gh":            {subs: []string{"pr", "issue", "run", "repo", "api"}},
	"docker":        {subs: []string{"ps", "images", "logs"}},
	"golangci-lint": {subs: []string{"run"}},
}

// simpleRewrite decides a plain one-segment command whose head is in simpleRules: "rtk "+cmd, or "" when excluded.
func simpleRewrite(cmd string) (string, bool) {
	if !plainLine(cmd) || !quietEnv() {
		return "", false
	}
	f, bare := shell.Tokens(cmd), bareWords(cmd)
	if bare == 0 || !ruleMatches(f, bare) {
		return "", false
	}
	excludes, ok := configExcludes()
	if !ok || slices.ContainsFunc(excludes, func(ex []string) bool { return len(ex) > bare && hasPrefix(f, ex) }) {
		return "", false
	}
	if slices.ContainsFunc(excludes, func(ex []string) bool { return hasPrefix(f, ex) }) {
		return "", true
	}
	return "rtk " + cmd, true
}

// ruleMatches reports f's head is a simple rule with enough arguments and, when it names subs, a bare one first.
func ruleMatches(f []string, bare int) bool {
	rule, ok := simpleRules[f[0]]
	if !ok || len(f)-1 < rule.minArgs {
		return false
	}
	return len(rule.subs) == 0 || bare >= 2 && slices.Contains(rule.subs, f[1])
}

// bareWords is how many leading words of cmd carry no quote: only those are matched by name against the rules.
func bareWords(cmd string) int {
	raw := strings.Fields(cmd)
	i := slices.IndexFunc(raw, func(w string) bool { return strings.ContainsAny(w, `'"`) })
	if i < 0 {
		return len(raw)
	}
	return i
}

// plainLine reports one segment of single-spaced words with nothing the shell expands or redirects outside quotes.
func plainLine(cmd string) bool {
	if cmd == "" || strings.ContainsAny(cmd, "$`\\\n\r\t") || strings.TrimSpace(cmd) != cmd {
		return false
	}
	var quote byte
	for i := 0; i < len(cmd); i++ {
		if quote = nextQuote(quote, cmd, i); quote == 0 && bareMeta(cmd, i) {
			return false
		}
	}
	return quote == 0
}

// nextQuote is the quote state after cmd[i]: a quote opens on ' or " and closes on its match.
func nextQuote(quote byte, cmd string, i int) byte {
	c := cmd[i]
	switch {
	case quote == 0 && (c == '\'' || c == '"'):
		return c
	case quote == c:
		return 0
	}
	return quote
}

// bareMeta reports cmd[i] an unquoted shell operator or a doubled space; callers pass only unquoted positions.
func bareMeta(cmd string, i int) bool {
	c := cmd[i]
	if c == '\'' || c == '"' {
		return false
	}
	return strings.IndexByte("|&;<>(){}#!", c) >= 0 || c == ' ' && i > 0 && cmd[i-1] == ' '
}

// quietEnv reports no RTK_* variable but the two the harness always sets: any other may change what rtk rewrites.
func quietEnv() bool {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "RTK_") && name != paths.RTKQuietEnv && name != paths.RTKTelemetryEnv {
			return false
		}
	}
	return true
}

func hasPrefix(words, prefix []string) bool {
	return len(words) >= len(prefix) && slices.Equal(words[:len(prefix)], prefix)
}

// nativeExcludes are the commands rtk rewrite leaves as typed whatever its config says.
var nativeExcludes = [][]string{{"git", "grep"}}

// harnessVetoes are the commands the harness never wraps (git commit), whatever rtk says.
var harnessVetoes = [][]string{{"git", "commit"}}

// configExcludes is rtk's exclude_commands as word lists; ok false when the config is absent or holds anything else.
func configExcludes() (excludes [][]string, ok bool) {
	if runtime.GOOS == "linux" && os.Getenv("XDG_CONFIG_HOME") != "" {
		return nil, false // rtk may read another config than paths.RTKConfig
	}
	file := paths.RTKConfig()
	if file == "" {
		return nil, false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	return parseExcludes(b)
}

// parseExcludes reads a config holding only [hooks] exclude_commands of plain word lists and [telemetry] enabled.
func parseExcludes(b []byte) (excludes [][]string, ok bool) {
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[hooks]" || line == "[telemetry]" {
			section = line
			continue
		}
		list, good := configLine(section, line)
		if !good {
			return nil, false
		}
		excludes = append(excludes, list...)
	}
	return excludes, sc.Err() == nil
}

// configLine is the word lists one key line of section holds; ok false for any key the fast path does not know.
func configLine(section, line string) (lists [][]string, ok bool) {
	key, value, _ := strings.Cut(line, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	switch {
	case section == "[telemetry]" && key == "enabled" && (value == "true" || value == "false"):
		return nil, true
	case section == "[hooks]" && key == "exclude_commands":
		return parseWordLists(value)
	}
	return nil, false
}

// plainPattern is an exclude pattern rtk reads as literal words; a regex character or one word is not decided here.
var plainPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z0-9_-]+( [A-Za-z0-9_=-]+)+$`)
})

// parseWordLists parses a one-line TOML array of plain-word strings into word lists.
func parseWordLists(value string) ([][]string, bool) {
	inner, ok := strings.CutPrefix(value, "[")
	if !ok {
		return nil, false
	}
	if inner, ok = strings.CutSuffix(inner, "]"); !ok {
		return nil, false
	}
	var out [][]string
	for item := range strings.SplitSeq(inner, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		word, opened := strings.CutPrefix(item, `"`)
		word, closed := strings.CutSuffix(word, `"`)
		if !opened || !closed || !plainPattern().MatchString(word) {
			return nil, false
		}
		out = append(out, strings.Fields(word))
	}
	return out, true
}

// plainHeads never run a command word they are given, so rtk leaves their segment as typed (verified on rtk 1.1.0).
var plainHeads = map[string]bool{"echo": true, "printf": true, "cd": true, "pwd": true, "mkdir": true, "rm": true,
	"cp": true, "mv": true, "touch": true, "chmod": true, "sort": true, "uniq": true, "cut": true, "tr": true,
	"sed": true, "awk": true, "true": true, "false": true, "sleep": true, "which": true, "type": true,
	"export": true, "tee": true, "basename": true, "dirname": true, "realpath": true, "readlink": true, "ln": true}

// noSegmentRewrites reports rtk leaves every segment of cmd as typed: no rtk word, a plain or rtk head, or excluded.
func noSegmentRewrites(cmd string) bool {
	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "<<") || strings.ContainsAny(cmd, "`\\") {
		return false
	}
	at := shell.QuoteAt(cmd)
	for i := range len(cmd) {
		if at[i] == 0 && strings.IndexByte("(){}", cmd[i]) >= 0 {
			return false
		}
	}
	for _, seg := range shell.Segments(cmd) {
		if !segmentStays(strings.Fields(seg)) {
			return false
		}
	}
	return true
}

// segmentStays reports one segment rtk leaves as typed.
func segmentStays(f []string) bool {
	if len(f) == 0 || plainHeads[f[0]] || f[0] == rtkName || !slices.ContainsFunc(f, func(w string) bool {
		return slices.ContainsFunc(wordRun().FindAllString(w, -1), isRewriteWord)
	}) {
		return true
	}
	fixed := slices.Concat(nativeExcludes, harnessVetoes)
	if slices.ContainsFunc(fixed, func(ex []string) bool { return hasPrefix(f, ex) }) || gitCommits(f) {
		return true
	}
	excludes, ok := configExcludes()
	return ok && slices.ContainsFunc(excludes, func(ex []string) bool { return hasPrefix(f, ex) })
}

// gitCommits reports a git segment whose subcommand is commit past global options (`git -c k=v commit`).
func gitCommits(f []string) bool {
	return len(f) > 0 && f[0] == "git" && gitRTKSubcommand(f[1:]) == "commit"
}
