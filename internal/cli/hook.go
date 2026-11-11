package cli

import (
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// hookSpec is the completion node for the hook verb.
func hookSpec() *helptext.Node { return &helptext.Node{Args: []func() []string{settings.HookEvents}} }

// hookPage lists modules in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
var hookPage = helptext.Page{
	Usage: "claude-code hook <Event> [--<module>|--all] < payload.json",
	About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the agent hook files invoke it.",
	Hint: settings.HookModules() + "\n" + hookEvents() +
		"\n<Event> alone runs the dispatcher; --<module> one module; --all every module",
}

// hookEvents wraps the registry's event names into help lines under 80 columns, as settings.HookModules does.
func hookEvents() string {
	var lines []string
	line := "events:  "
	for _, ev := range settings.HookEvents() {
		if len(line)+len(ev)+1 > 79 {
			lines = append(lines, line)
			line = "         "
		}
		line += ev + "|"
	}
	return strings.Join(append(lines, strings.TrimSuffix(line, "|")), "\n")
}

// cmdHook runs one lifecycle hook.
func cmdHook(args []string) int {
	if len(args) == 0 || isHelpVerb(args[0]) {
		hookPage.Fprint(os.Stderr)
		return 0
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}
