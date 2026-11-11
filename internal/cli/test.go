// The `test` verb: test tooling.

package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/hosttest/ramdisk"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdTest dispatches `test <sub>`.
func cmdTest(args []string) int {
	if len(args) > 0 && args[0] == "ramdisk" {
		return ramdisk.Run(args[1:])
	}
	if len(args) == 0 || isHelpVerb(args[0]) {
		testPage.Fprint(os.Stderr)
		return 0
	}
	helptext.Fail("test: unknown subcommand %q", args[0])
	testPage.Fprint(os.Stderr)
	return 2
}

var testPage = helptext.Page{
	Usage: "claude-code test <command>",
	About: "Test tooling.",
	Commands: [][2]string{
		{"ramdisk", "run a command with TMPDIR on a throwaway RAM disk"},
	},
}

// testSpec is the completion node for the test verb tree.
func testSpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ramdisk": {},
	}}
}
