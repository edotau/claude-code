// Command harness-hook builds bin/.hookd, the hooks-only harness binary; other verbs hand over to claude-code beside it.
package main

import (
	"os"

	"github.com/edotau/claude-code/internal/hooks"

	// the registries hook modules read: ask's flag schema and the agent roster with its drivers
	_ "github.com/edotau/claude-code/internal/agent/ask"
	_ "github.com/edotau/claude-code/internal/driver/claude"
	_ "github.com/edotau/claude-code/internal/driver/codex"
	_ "github.com/edotau/claude-code/internal/driver/copilot"
	_ "github.com/edotau/claude-code/internal/driver/opencode"
	_ "github.com/edotau/claude-code/internal/driver/pi"
	_ "github.com/edotau/claude-code/internal/memory/shadow"
)

func main() { os.Exit(hooks.Main(os.Args)) }
