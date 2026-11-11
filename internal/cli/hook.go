package cli

import (
	"os"

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
	Hint:  settings.HookModules() + "\nevents: <Event> runs the dispatcher, --<module> one module, --all every module",
}

// cmdHook runs one lifecycle hook.
func cmdHook(args []string) int {
	if len(args) == 0 || isHelpVerb(args[0]) {
		hookPage.Fprint(os.Stderr)
		return 0
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}
