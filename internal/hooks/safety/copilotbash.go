// `hook copilot-bash` — the copilot subagent's frontmatter PreToolUse(Bash) guard: only the scoped shell allowlist runs.

package safety

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// CopilotBashHook is the `claude-code hook <name>` spelling agents/copilot.md carries; it is not a settings.json event.
const CopilotBashHook = "copilot-bash"

// copilotBashReason names the hook's allowlist, generated from the copilot shell policy.
func copilotBashReason() string {
	p := agentconfig.ShellPolicy()
	return "copilot-bash: BLOCKED — Bash is limited to " + p.Commands.Summary() + ", 2>&1, paths in the cwd or ~/worktrees, " +
		p.HookExtras.Summary() + "."
}

// CopilotBash reads one hook JSON from stdin and exits 2 unless every chained segment is allowed (fail closed).
func CopilotBash(stdin io.Reader, stderr io.Writer) int {
	var in struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
		CWD string `json:"cwd"`
	}
	raw, err := io.ReadAll(stdin)
	if err != nil || json.Unmarshal(raw, &in) != nil || strings.TrimSpace(in.ToolInput.Command) == "" {
		fmt.Fprintln(stderr, "copilot-bash: BLOCKED — unreadable hook input or no command.")
		return core.ExitBlock
	}
	cmd, workDir := in.ToolInput.Command, in.CWD
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	cmd = shell.CanonicalSelf(cmd)
	home, _ := os.UserHomeDir() // ~/worktrees holds the linked worktrees a ~/.claude session works in
	if !shell.CommandIn(cmd, workDir, []string{workDir, filepath.Join(home, "worktrees")}, agentconfig.ShellPolicy().HookExtras) {
		fmt.Fprintln(stderr, copilotBashReason())
		return core.ExitBlock
	}
	return core.ExitProceed
}
