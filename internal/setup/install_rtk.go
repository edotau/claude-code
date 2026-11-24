package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	Glob: paths.RTKConfig, Keep: "rtk-owned config outside the harness tree",
})

// installRTKConfig writes rtkConfig only where no file exists; any other content, marker or not, is the user's.
func installRTKConfig(w io.Writer, dryRun bool) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file := paths.RTKConfig()
	if file == "" {
		return errors.New("rtk config: cannot resolve the home directory")
	}
	old, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read rtk config: %w", err)
	}
	switch {
	case err == nil && bytes.Equal(old, []byte(rtkConfig)):
		fmt.Fprintf(w, "rtk-config %s\n", paths.ShortHome(file))
		return nil
	case err == nil:
		fmt.Fprintf(w, "rtk-config kept user file %s\n", paths.ShortHome(file))
		return nil
	case dryRun:
		fmt.Fprintf(w, "rtk-config would write %s (skipped: --dry-run)\n", paths.ShortHome(file))
		return nil
	}
	if _, err := paths.WriteIfUnchanged(file, nil, []byte(rtkConfig), 0o644, "rtk config"); err != nil {
		return fmt.Errorf("write rtk config: %w", err)
	}
	fmt.Fprintf(w, "rtk-config %s\n", paths.ShortHome(file))
	return nil
}
