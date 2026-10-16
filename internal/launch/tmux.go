package launch

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

//go:embed tmux.conf
var tmuxConf []byte

// tmuxConfPaths are the files tmux reads at startup; the first is where a missing config is seeded.
var tmuxConfPaths = func() []string {
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	return []string{filepath.Join(home, ".tmux.conf"), filepath.Join(xdg, "tmux", "tmux.conf")}
}

// seedTmuxConf writes the harness tmux.conf only when no tmux config exists; a present one is never touched.
func seedTmuxConf(w io.Writer, dryRun bool) {
	paths := tmuxConfPaths()
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintf(w, "tmux     %s exists (kept)\n", p)
			return
		}
	}
	if dryRun {
		fmt.Fprintf(w, "tmux     would write %s (skipped: --dry-run)\n", paths[0])
		return
	}
	if err := os.WriteFile(paths[0], tmuxConf, 0o644); err != nil {
		fmt.Fprintf(w, "tmux     skip  %v\n", err)
		return
	}
	fmt.Fprintf(w, "tmux     wrote %s\n", paths[0])
}
