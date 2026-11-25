package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func cmdMemoryExtract(args []string) int {
	fs := memoryFlagSet("extract")
	dry := fs.Bool("dry-run", false, "classify and list purge candidates; write nothing in the store")
	purge := fs.Bool("purge", false, "purge only proven-dead files in one local commit; no model")
	format := fs.String("format", "text", "output format: text|json")
	out := fs.String("out", "", "write typed extract.json rows outside the store")
	if code, done := memoryParseNoArgs(fs, "extract", args); done {
		return code
	}
	if !*dry && !*purge {
		return helptext.FailUsage("memory extract: specify --dry-run or --purge")
	}
	if *format != "text" && *format != "json" {
		return helptext.FailUsage("memory extract: --format must be text or json")
	}
	if err := memory.RunExtract(memory.ExtractRunOptions{DryRun: *dry, Purge: *purge, Format: *format, Out: *out}, os.Stdout); err != nil {
		return helptext.Fail("memory extract: %v", err)
	}
	return 0
}
