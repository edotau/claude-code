// Command harness-hook builds bin/.hookd, the hooks-only harness binary; other verbs hand over to claude-code beside it.
package main

import (
	"os"

	"github.com/edotau/claude-code/internal/hooks"

	// the registries hook modules read: ask's flag schema (askspec) and the agent roster with its drivers
	_ "github.com/edotau/claude-code/internal/agent/ask"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/opencode"
	_ "github.com/edotau/claude-code/internal/pi"
)

func main() { os.Exit(hooks.Main(os.Args)) }
