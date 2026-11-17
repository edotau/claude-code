package project

import (
	"bytes"
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
	opts := Options{Root: root, Home: filepath.Join(scratch, "home"), StateDir: filepath.Join(scratch, "state"), Out: io.Discard, write: writeScratch}
	if rc := Sync(opts); rc != 0 {
		return fmt.Errorf("fresh render exits %d", rc)
	}
	opts.Check = true
	if rc := Sync(opts); rc != 0 {
		return fmt.Errorf("second render is not a no-op (exit %d)", rc)
	}
	return nil
}

// writeScratch skips the temp file and fsync that WriteIfChanged pays; the scratch tree is deleted right after.
func writeScratch(dest string, data []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(dest); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return true, err
	}
	return true, os.WriteFile(dest, data, mode)
}
