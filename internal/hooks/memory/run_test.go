package memory

import (
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/settings"
)

var runners = map[string]core.Runner{
	"memory-start": Start, "memory-recall": Recall, "memory-update": Stop, "memory-end": End, "memory-compact": Compact,
}

// runDirect is the `hook <Event> --<module>` render of one memory runner, as package hooks does it.
func runDirect(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	in := core.ParseInput(stdin)
	res := runners[name](in)
	if res.Decision == core.Block {
		fmt.Fprintln(stderr, res.Reason)
		return core.ExitBlock
	}
	event := ""
	for _, s := range settings.Specs() {
		if s.Name == name {
			event = s.Event
		}
	}
	return core.Emit(in.Agent(), event, res, stdout, stderr)
}
