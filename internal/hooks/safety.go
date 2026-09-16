package hooks

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

// hardBlocks are command shapes that are almost never intentional.
var hardBlocks = lazyCompile(
	`rm\s+-(rf|fr|r)\s+/(\*|\s|$)`,
	`rm\s+-(rf|fr|r)\s+/(etc|usr|var|bin|sbin|lib|System|Users|home)(/?\s|/?$)`,
	`rm\s+-(rf|fr|r)\s+(\$HOME|\$\{HOME\}|~)/?(\*|\s|$)`,
	`mkfs\.`,
	`dd\s+.*\sof=/dev/(sd|disk|nvme|hd)`,
	`chmod\s+-R\s+777\s+/(\s|$)`,
	`:\(\)\s*\{.*\};\s*:`,
	`(^|[^[:alnum:]_])sudo(\s|$)`,
	`git\s+push\s+.*(--force|-f)(\s|$).*(main|master)(\s|$)`,
	`git\s+push\s+.*(main|master)(\s|$).*(--force|-f)(\s|$)`,
	`(^|[^[:alnum:]_])(curl|wget)\s[^|]*\|\s*(ba|z)?sh([^[:alnum:]_]|$)`,
	`python3?\s+.*-c\s+.*exec\(.*base64`,
)

// sqlDestructive runs against the lowercased command.
var sqlDestructive = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^[:alnum:]_])(drop\s+database|drop\s+table|truncate\s+table)([^[:alnum:]_]|$)`)
})

// exfilPatterns match credential-exfiltration shapes.
var exfilPatterns = lazyCompile(
	`(env|printenv)\s*>\s*/tmp/`,
	`(cat|tail|head|base64)\s+.*(\.claude/settings\.json|\.credentials\.json|secrets\.env|\.ssh/|\.aws/credentials|\.netrc).*\|.*(^|[^[:alnum:]_])(curl|nc|ncat|wget)([^[:alnum:]_]|$)`,
	`base64\s+.*(\.ssh/id_|\.aws/credentials|\.credentials\.json|secrets\.env)`,
	`(scp|rsync)\s+.*\.ssh/id_`,
)

// softWarn patterns print a warning but never block.
var softWarn = lazyCompile(
	`kubectl\s+delete(\W|$)`,
	`docker\s+system\s+prune`,
	`git\s+reset\s+--hard`,
	`git\s+clean\s+-[a-z]*f`,
)

func lazyCompile(pats ...string) func() []*regexp.Regexp {
	return sync.OnceValue(func() []*regexp.Regexp {
		out := make([]*regexp.Regexp, len(pats))
		for i, p := range pats {
			out[i] = regexp.MustCompile(p)
		}
		return out
	})
}

// Safety is the PreToolUse(Bash) guard: block destructive and exfiltration commands.
func Safety(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	if in.ToolName != "" && in.ToolName != "Bash" {
		return ExitProceed
	}
	cmd := in.ToolCommand()
	if cmd == "" {
		return ExitProceed
	}
	for _, re := range hardBlocks() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: BLOCKED — destructive command\n  pattern: %s\n  command: %s\nIf intentional, run it yourself in a terminal.\n", re, cmd)
			return ExitBlock
		}
	}
	if sqlDestructive().MatchString(strings.ToLower(cmd)) {
		fmt.Fprintf(stderr, "safety: BLOCKED — destructive SQL (DROP/TRUNCATE)\n  command: %s\n", cmd)
		return ExitBlock
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: BLOCKED — looks like credential exfiltration\n  pattern: %s\n  command: %s\n", re, cmd)
			return ExitBlock
		}
	}
	for _, re := range softWarn() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: WARNING — %s (not blocked; review carefully)\n", re)
		}
	}
	return ExitProceed
}
