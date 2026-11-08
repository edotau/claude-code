package clipboard

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Clip saves the Windows clipboard image as a PNG and prints its WSL path, for @-reference when
// WSL2 Ctrl+V image paste fails on the bridged BMP.
func Clip(args []string) int {
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
		if err := ServePNG(os.Stdout); err != nil {
			return helptext.Fail("clip --serve: %v", err)
		}
		return 0
	}
	path, dim, err := Grab()
	if err != nil {
		return helptext.Fail("clip: %v", err)
	}
	fmt.Fprintf(os.Stderr, "clipboard image %s saved\n", dim)
	fmt.Println(path)
	return 0
}

// WLPaste is the WSL wl-paste proxy; args are wl-paste's own and pass through verbatim, --help included.
func WLPaste(args []string) int {
	return RunProxy(args, os.Stdout, os.Stderr)
}

// ClipClear sleeps --after, then clears the clipboard if it still holds the value whose sha256
// digest arrives on stdin; ScheduleClear spawns it detached.
func ClipClear(args []string) int {
	fs := helptext.NewFlagSet("clip-clear", helptext.Page{
		Usage: "claude-code clip-clear [--after 30s] < digest",
		About: "Waits, then clears the clipboard only if it still holds the copied secret (sha256 on stdin).",
	})
	after := fs.Duration("after", DefaultClearAfter, "delay before the clear")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 256))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return helptext.Fail("clip-clear: no digest on stdin")
	}
	time.Sleep(*after)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second) // a hung clipboard tool must not block the clear
	defer cancel()
	if _, err := ClearIfMatch(ctx, string(b)); err != nil {
		return helptext.Fail("clip-clear: %v", err)
	}
	return 0
}

// Spec is the completion node for `clip`.
func Spec() *completion.Node {
	return &completion.Node{Flags: []completion.Flag{{Name: "serve", IsBool: true}}}
}
