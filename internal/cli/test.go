// The `test` verb: test tooling that checks a test has teeth.

package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/testtool/mutate"
	"github.com/edotau/claude-code/internal/testtool/ramdisk"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdTest dispatches `test <sub>`.
func cmdTest(args []string) int {
	if len(args) > 0 && args[0] == "mutate" {
		return mutate.Run(args[1:])
	}
	if len(args) > 0 && args[0] == "ramdisk" {
		return ramdisk.Run(args[1:])
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		testPage.Fprint(os.Stderr)
		return 0
	}
	helptext.Fail("test: unknown subcommand %q", args[0])
	testPage.Fprint(os.Stderr)
	return 2
}

var testPage = helptext.Page{
	Usage: "claude-code test <command> [flags]",
	Commands: [][2]string{
		{"mutate", "swap one snippet via -overlay; do the tests notice?"},
		{"ramdisk", "run a command with TMPDIR on a throwaway RAM disk"},
	},
	Hint: "claude-code test mutate -h lists that command's flags.",
}

// testSpec is the completion node for the test verb tree.
func testSpec() *completion.Node {
	return &completion.Node{Sub: map[string]*completion.Node{
		"mutate":  {Flags: []completion.Flag{{Name: "run"}}},
		"ramdisk": {},
	}}
}
