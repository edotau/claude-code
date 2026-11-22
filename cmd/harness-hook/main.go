// Command harness-hook builds bin/.hooks, the hooks-only harness binary; other verbs hand over to claude-code beside it.
package main

import (
	"os"

	"github.com/edotau/claude-code/internal/hooks"
)

func main() { os.Exit(hooks.Main(os.Args)) }
