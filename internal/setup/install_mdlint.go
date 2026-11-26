package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/mdlint"
	"github.com/edotau/claude-code/internal/paths"
)

// installMdlintConfig refreshes the memory store's .markdownlint.json child config; a missing or symlinked store skips.
func installMdlintConfig(w io.Writer, dryRun bool) {
	dir := paths.MemoryStoreDir()
	dest := filepath.Join(dir, mdlint.RulesFile)
	rel := "memory/" + mdlint.RulesFile
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		fmt.Fprintf(w, "mdlint   %s skip  no memory store\n", rel)
		return
	}
	if _, err := os.Stat(filepath.Join(paths.ConfigDir(), mdlint.RulesFile)); err != nil {
		fmt.Fprintf(w, "mdlint   %s skip  no parent %s\n", rel, mdlint.RulesFile)
		return
	}
	want := mdlint.StoreRules("memory")
	if dryRun {
		status := "would write"
		if have, err := os.ReadFile(dest); err == nil && bytes.Equal(have, want) {
			status = "ok"
		}
		fmt.Fprintf(w, "mdlint   %s %s %s (skipped: --dry-run)\n", rel, status, paths.ShortHome(dest))
		return
	}
	wrote, err := paths.WriteIfChanged(dest, want, 0o644)
	if err != nil {
		fmt.Fprintf(w, "mdlint   %s skip  %v\n", rel, err)
		return
	}
	status := "ok"
	if wrote {
		status = "written"
	}
	fmt.Fprintf(w, "mdlint   %s %s\n", rel, status)
}
