package dispatchhook

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
)

var relayOutputMerge = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[ \t]+2>&1[ \t]*$`) })

// relayWords shares wrapper handling without changing the safety scanner's Unwrap behavior.
func relayWords(seg string) []string { return unwrapRelay(shell.Tokens(seg)) }

func unwrapRelay(words []string) []string {
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
	if len(words) < 3 || !shell.IsHarnessName(filepath.Base(words[0])) {
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
	if words[0] == "agents" && slices.Contains([]string{"ask", "result"}, words[1]) {
		return words[1]
	}
	return ""
}

// reviewVerb is `claude-code <external agent> review`: a vendor run that is no ask, so no job id is waited on.
func reviewVerb(words []string) bool {
	words = agentCommand(words)
	return len(words) > 1 && externalAgent(words[0]) && words[1] == "review"
}

// relayInput permits only literal argv and one stdin file redirect; no expansions or shell execution.
func relayInput(seg string) (words []string, input string, err error) {
	if !relayQuotesClosed(seg) {
		return nil, "", errors.New("has an unclosed quote or escape")
	}
	at := shell.QuoteAt(seg)
	redirect := -1
	for i, c := range []byte(seg) {
		if err := relayByte(seg, at, i); err != nil {
			return nil, "", err
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
	return relayRedirect(seg, at, redirect)
}

// relayRedirect splits seg at its `< file` redirect into the argv words and the file.
func relayRedirect(seg string, at []byte, redirect int) ([]string, string, error) {
	files := shell.Tokens(seg[redirect+1:])
	if len(files) != 1 || files[0] == "" {
		return nil, "", errors.New("redirect is not a literal `< file`")
	}
	if j := len(seg) - len(strings.TrimLeft(seg[redirect+1:], " \t")); at[j] == 0 && seg[j] == '~' {
		files[0] = paths.ExpandHome(files[0]) // the shell expands it; the brief is read from the real path
	}
	return shell.Tokens(seg[:redirect]), files[0], nil
}

// relayByte refuses an expansion, or an unquoted operator other than a HOME tilde, at seg[i].
func relayByte(seg string, at []byte, i int) error {
	c := seg[i]
	if at[i] == '\'' {
		return nil // single-quoted text is literal
	}
	if c == '`' || c == '$' {
		return errors.New("contains `$` or a backtick")
	}
	if at[i] != 0 || c == '~' && relayHomeTilde(seg, i) {
		return nil
	}
	if strings.ContainsRune(";|&(){}>*?[]~#", rune(c)) {
		return fmt.Errorf("contains an unquoted shell operator `%c`", c)
	}
	return nil
}

// relayHomeTilde is a `~` the shell expands to HOME: at a word start or after `=`, then `/` or the word's end.
func relayHomeTilde(seg string, i int) bool {
	if i > 0 && !strings.ContainsRune(" \t=", rune(seg[i-1])) {
		return false
	}
	return i+1 == len(seg) || strings.ContainsRune("/ \t", rune(seg[i+1]))
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
