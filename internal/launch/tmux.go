package launch

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed tmux.conf
var tmuxConf []byte

// tmuxConfPaths are the files tmux reads at startup; the first is where a missing config is seeded.
var tmuxConfPaths = func() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	return []string{filepath.Join(home, ".tmux.conf"), filepath.Join(xdg, "tmux", "tmux.conf")}
}

// seedTmuxConf writes the harness tmux.conf only when no tmux config exists; a present one is never touched.
func seedTmuxConf(w io.Writer, dryRun bool) {
	paths := tmuxConfPaths()
	if len(paths) == 0 {
		fmt.Fprintln(w, "tmux     skip  home directory unresolved")
		return
	}
	for _, p := range paths {
		_, err := os.Stat(p)
		if err == nil {
			fmt.Fprintf(w, "tmux     %s exists (kept)\n", p)
			return
		}
		if !errors.Is(err, fs.ErrNotExist) { // unknown state: never seed over a config we could not probe
			fmt.Fprintf(w, "tmux     skip  %v\n", err)
			return
		}
	}
	if dryRun {
		fmt.Fprintf(w, "tmux     would write %s (skipped: --dry-run)\n", paths[0])
		return
	}
	f, err := os.OpenFile(paths[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // O_EXCL: never truncate a racing writer's config
	if errors.Is(err, fs.ErrExist) {
		fmt.Fprintf(w, "tmux     %s exists (kept)\n", paths[0])
		return
	}
	if err == nil {
		_, err = f.Write(tmuxConf)
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		fmt.Fprintf(w, "tmux     skip  %v\n", err)
		return
	}
	fmt.Fprintf(w, "tmux     wrote %s\n", paths[0])
}
