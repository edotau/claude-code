package cli

import (
	"fmt"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/rtkwrap"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdAgentsCodemode is `agents codemode on|off|status [--session ID]`.
func cmdAgentsCodemode(args []string) int {
	fs := agentsFlags("codemode")
	session := fs.String("session", os.Getenv(paths.SessionIDEnv), "the Claude session id (default: this session)")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || !slices.Contains([]string{"on", "off", "status"}, pos[0]) {
		fs.Usage()
		return 2
	}
	if *session == "" {
		return helptext.FailUsage("agents codemode: no session: pass --session or run inside a Claude session")
	}
	if pos[0] != "status" {
		if err := rtkwrap.SetSessionFlag(*session, pos[0] == "on"); err != nil {
			return helptext.FailUsage("agents codemode: %v", err)
		}
	}
	line, err := rtkwrap.CodeModeStatus(*session)
	if err != nil {
		return helptext.FailUsage("agents codemode: %v", err)
	}
	fmt.Println(line)
	return 0
}
