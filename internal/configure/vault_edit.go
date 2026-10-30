package configure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"

	"github.com/edotau/claude-code/internal/tui"
)

// isTerminal and confirmEdit are seams for the re-edit prompt.
var isTerminal = tui.StdinIsTerminal
var confirmEdit = func(q string) bool { return tui.ConfirmKey(q, true) }

// Edit opens the non-secret config in $VISUAL/$EDITOR via a 0600 temp file and imports it; the file is always removed.
func Edit(stdout, stderr io.Writer) error {
	orig, err := Export()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "claude-code-config-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() // best-effort cleanup of the temp file
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(orig); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	argv := strings.Fields(os.Getenv("VISUAL"))
	if len(argv) == 0 {
		argv = strings.Fields(os.Getenv("EDITOR"))
	}
	if len(argv) == 0 {
		argv = []string{"vi"}
		if runtime.GOOS == "windows" {
			argv[0] = "notepad"
		}
	}
	for {
		cmd := exec.CommandContext(context.Background(), argv[0], append(argv[1:], f.Name())...) // top-level: interactive editor, no caller ctx
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		signal.Ignore(os.Interrupt)
		err := cmd.Run()
		signal.Reset(os.Interrupt)
		if err != nil {
			return fmt.Errorf("editor %s: %w", argv[0], err)
		}
		edited, err := os.ReadFile(f.Name())
		if err != nil {
			return err
		}
		if bytes.Equal(edited, orig) {
			fmt.Fprintln(stdout, "no changes")
			return nil
		}
		err = Import(orig, edited)
		if err == nil {
			fmt.Fprintln(stdout, "saved")
			return nil
		}
		if errors.Is(err, ErrChanged) {
			return err
		}
		if !isTerminal() {
			return err
		}
		fmt.Fprintln(stderr, "config:", err)
		if !confirmEdit("Re-edit?") {
			return err
		}
	}
}
