// Command harness-hook builds bin/.hookd, the hooks-only harness binary; other verbs hand over to claude-code beside it.
package main

import (
	"os"

	"github.com/edotau/claude-code/internal/hooks"

	// Hook modules need the agent roster and route registrations, not the SDK runtime.
	_ "github.com/edotau/claude-code/internal/driver/claude"
	_ "github.com/edotau/claude-code/internal/driver/codex"
	_ "github.com/edotau/claude-code/internal/driver/copilot"
	_ "github.com/edotau/claude-code/internal/driver/opencode"
	_ "github.com/edotau/claude-code/internal/driver/pi"
	_ "github.com/edotau/claude-code/internal/memory/shadow"
)

func main() { os.Exit(hooks.Main(os.Args)) }
