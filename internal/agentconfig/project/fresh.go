package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CheckFreshRender renders root's sources twice into a scratch home and state; the live homes are never touched.
func CheckFreshRender(root string) error {
	scratch, err := os.MkdirTemp("", "claude-code-render-check-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	opts := Options{Root: root, Home: filepath.Join(scratch, "home"), StateDir: filepath.Join(scratch, "state"), Out: io.Discard}
	if rc := Sync(opts); rc != 0 {
		return fmt.Errorf("fresh render exits %d", rc)
	}
	opts.Check = true
	if rc := Sync(opts); rc != 0 {
		return fmt.Errorf("second render is not a no-op (exit %d)", rc)
	}
	return nil
}
