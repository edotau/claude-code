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

// cmdClip saves the clipboard image as a PNG and prints its path; --serve, --clear and `paste` are its helper modes.
func cmdClip(args []string) int {
	if len(args) > 0 && args[0] == "paste" { // wl-paste's own args pass through verbatim, --help included
		return clipboard.RunProxy(args[1:], os.Stdout, os.Stderr)
	}
	fs := helptext.NewFlagSet("clip", helptext.Page{
		Usage: "claude-code clip [--serve | --clear [--after 30s] | paste ...]",
		About: "Saves the clipboard image to a PNG and prints its path (macOS, WSL); hand it to ask --attach.\n" +
			"--clear waits, then clears the clipboard if it still matches the sha256 digest on stdin.\n" +
			"paste is the WSL wl-paste proxy: answers agent image paste, else runs the real wl-paste.",
	})
	serve := fs.Bool("serve", false, "write the macOS clipboard PNG to stdout (launchd bridge)")
	clear := fs.Bool("clear", false, "clear the clipboard after --after if it still matches the digest on stdin")
	after := fs.Duration("after", clipboard.DefaultClearAfter, "with --clear: delay before the clear")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *clear {
		return clipClear(*after)
	}
	if *serve { // stdout is the bridge socket: errors go to stderr only
		if err := clipboard.ServePNG(os.Stdout); err != nil {
			return helptext.Fail("clip --serve: %v", err)
		}
		return 0
	}
	path, dim, err := clipboard.Grab()
	if err != nil {
		return helptext.Fail("clip: %v", err)
	}
	fmt.Fprintf(os.Stderr, "clipboard image %s saved\n", dim)
	fmt.Println(path)
	return 0
}

func clipClear(after time.Duration) int {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 256))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return helptext.Fail("clip --clear: no digest on stdin")
	}
	time.Sleep(after)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second) // a hung clipboard tool must not block the clear
	defer cancel()
	if _, err := clipboard.ClearIfMatch(ctx, string(b)); err != nil {
		return helptext.Fail("clip --clear: %v", err)
	}
	return 0
}

// clipSpec is the completion node for `clip`.
func clipSpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{"paste": {}},
		Flags: []helptext.Flag{{Name: "serve", IsBool: true}, {Name: "clear", IsBool: true}, helptext.Plain("after")}}
}
