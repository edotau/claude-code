package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

const rtkConfigMarker = "# Rendered by the claude-code harness.\n"

const rtkConfig = rtkConfigMarker + `
[hooks]
exclude_commands = ["go vet", "go test -v", "go test -list"]

[telemetry]
enabled = false
`

var _ = schemas.Register(schemas.Format{
	Name: "rtk-config", Owner: "internal/setup", Kind: schemas.Derived, Version: 1,
	Glob: rtkConfigPath, Keep: "rtk-owned config outside the harness tree",
})

func rtkConfigPath() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "rtk", "config.toml")
	}
	return filepath.Join(home, ".config", "rtk", "config.toml")
}

func installRTKConfig(w io.Writer, dryRun bool) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file := rtkConfigPath()
	if file == "" {
		return errors.New("rtk config: cannot resolve the home directory")
	}
	old, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read rtk config: %w", err)
	}
	if err == nil && !bytes.HasPrefix(old, []byte(rtkConfigMarker)) {
		fmt.Fprintf(w, "rtk-config kept user file %s\n", paths.ShortHome(file))
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "rtk-config would write %s (skipped: --dry-run)\n", paths.ShortHome(file))
		return nil
	}
	if !bytes.Equal(old, []byte(rtkConfig)) {
		if _, err := paths.WriteIfUnchanged(file, old, []byte(rtkConfig), 0o644, "rtk config"); err != nil {
			return fmt.Errorf("write rtk config: %w", err)
		}
	}
	fmt.Fprintf(w, "rtk-config %s\n", paths.ShortHome(file))
	return nil
}
