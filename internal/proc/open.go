package proc

import (
	"context"
	"os/exec"
	"runtime"
)

// wslExplorer opens a URL in the Windows default browser; absolute because ssh sessions lack the Windows PATH.
const wslExplorer = "/mnt/c/Windows/explorer.exe"

// Seams: tests swap the OS and the WSL answer to reach every opener from any host.
var (
	openGOOS = runtime.GOOS
	openWSL  = OnWSL
)

// OpenURL starts the desktop opener without waiting: explorer.exe exits 1 even when it opens the page.
func OpenURL(url string) error {
	return exec.CommandContext(context.Background(), Opener(), url).Start() // the opener outlives this call
}

// Opener is this machine's URL opener: open on macOS, wslview or explorer.exe on WSL, else xdg-open.
func Opener() string {
	switch {
	case openGOOS == "darwin":
		return "open"
	case openWSL():
		if p, err := exec.LookPath("wslview"); err == nil {
			return p
		}
		return wslExplorer
	}
	return "xdg-open"
}
