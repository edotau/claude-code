package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/helptext"
)

// cmdClip saves the Windows clipboard image as a PNG and prints its WSL path, for @-reference when
// WSL2 Ctrl+V image paste fails on the bridged BMP.
func cmdClip(args []string) int {
	fs := helptext.NewFlagSet("clip", helptext.Page{
		Usage: "claude-code clip",
		About: "Saves the clipboard image to a PNG and prints its path (macOS, WSL). " +
			"Fallback for Ctrl+V image paste; hand the path to ask --attach.",
	})
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	path, dim, err := clipboard.Grab()
	if err != nil {
		return fail("clip: %v", err)
	}
	fmt.Fprintf(os.Stderr, "clipboard image %s saved\n", dim)
	fmt.Println(path)
	return 0
}

// cmdBMPTranscode pipes a WSLg-bridged BMP (stdin) to a decodable 24bpp BMP, or PNG with --png
// (stdout); the wl-paste proxy calls it so native image paste can decode what WSLg hands over.
func cmdBMPTranscode(args []string) int {
	fs := helptext.NewFlagSet("bmp-transcode", helptext.Page{
		Usage: "claude-code bmp-transcode [flags] < in.bmp > out",
		About: "Re-encode a WSLg-bridged BMP on stdin as a decodable 24bpp BMP on stdout; " +
			"the wl-paste proxy calls it so native image paste works.",
	})
	asPNG := fs.Bool("png", false, "emit PNG instead of BMP (the image/png paste leg)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	transcode := clipboard.Transcode
	if *asPNG {
		transcode = clipboard.TranscodePNG
	}
	if err := transcode(os.Stdin, os.Stdout); err != nil {
		return fail("bmp-transcode: %v", err)
	}
	return 0
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
