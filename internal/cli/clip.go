package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdClip saves the Windows clipboard image as a PNG and prints its WSL path, for @-reference when
// WSL2 Ctrl+V image paste fails on the bridged BMP.
func cmdClip(args []string) int {
	fs := helptext.NewFlagSet("clip", helptext.Page{
		Usage: "claude-code clip [--serve]",
		About: "Saves the clipboard image to a PNG and prints its path (macOS, WSL). " +
			"Fallback for Ctrl+V image paste; hand the path to ask --attach.",
	})
	serve := fs.Bool("serve", false, "write the macOS clipboard PNG to stdout (launchd bridge)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *serve { // stdout is the bridge socket: errors go to stderr only
		if err := clipboard.ServePNG(os.Stdout); err != nil {
			return fail("clip --serve: %v", err)
		}
		return 0
	}
	path, dim, err := clipboard.Grab()
	if err != nil {
		return fail("clip: %v", err)
	}
	fmt.Fprintf(os.Stderr, "clipboard image %s saved\n", dim)
	fmt.Println(path)
	return 0
}

// cmdWLPaste is the WSL wl-paste proxy; args are wl-paste's own and pass through verbatim, --help included.
func cmdWLPaste(args []string) int {
	return clipboard.RunProxy(args, os.Stdout, os.Stderr)
}

// cmdClipClear sleeps --after, then clears the clipboard if it still holds the value whose sha256
// digest arrives on stdin; ScheduleClear spawns it detached.
func cmdClipClear(args []string) int {
	fs := helptext.NewFlagSet("clip-clear", helptext.Page{
		Usage: "claude-code clip-clear [--after 30s] < digest",
		About: "Waits, then clears the clipboard only if it still holds the copied secret (sha256 on stdin).",
	})
	after := fs.Duration("after", clipboard.DefaultClearAfter, "delay before the clear")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 256))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return fail("clip-clear: no digest on stdin")
	}
	time.Sleep(*after)
	if _, err := clipboard.ClearIfMatch(context.Background(), string(b)); err != nil {
		return fail("clip-clear: %v", err)
	}
	return 0
}
