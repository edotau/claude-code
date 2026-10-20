package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/helptext"
)

// cmdClip saves the Windows clipboard image as a PNG and prints its WSL path, for @-reference when
// WSL2 Ctrl+V image paste fails on the bridged BMP.
func cmdClip(args []string) int {
	fs := flag.NewFlagSet("clip", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage: claude-code clip\n"+
			"  Saves the Windows clipboard image to a PNG and prints its WSL path.\n"+
			"  Fallback for WSL2 Ctrl+V image paste.\n")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
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
	fs := flag.NewFlagSet("bmp-transcode", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of bmp-transcode (stdin BMP → stdout):\n")
		helptext.Defaults(fs)
	}
	asPNG := fs.Bool("png", false, "emit PNG instead of BMP (the image/png paste leg)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
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
