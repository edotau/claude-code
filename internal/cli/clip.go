package cli

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/helptext"
)

// cmdClip saves the Windows clipboard image as a PNG and prints its WSL path, for @-reference when
// WSL2 Ctrl+V image paste fails on the bridged BMP.
func cmdClip(args []string) int {
	fs := helptext.NewFlagSet("clip", helptext.Page{
		Usage: "claude-code clip",
		About: "Saves the Windows clipboard image to a PNG and prints its WSL path. " +
			"Fallback for WSL2 Ctrl+V image paste.",
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
