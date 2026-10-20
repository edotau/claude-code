//go:build unix

package gemini

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO sitting in a scanned dir must be skipped by its directory-entry type alone (fs.DirEntry's
// Type() comes from the readdir syscall, no open needed), never opened — opening a FIFO with no
// writer blocks forever, so a regression here would hang the whole scan rather than just mis-skip.
func TestWalkDirsSkipsFIFO(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "real.txt"), "hello")
	fifoPath := filepath.Join(dir, "a.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan struct {
		ctx Context
		err error
	}, 1)
	go func() {
		ctx, err := CollectContext(t.Context(), dir, []string{"."}, nil, 40, 100)
		done <- struct {
			ctx Context
			err error
		}{ctx, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("CollectContext err = %v", r.err)
		}
		for _, inc := range r.ctx.Included {
			if inc.Path == "a.fifo" {
				t.Fatalf("FIFO was opened and included: %+v", inc)
			}
		}
		found := false
		for _, s := range r.ctx.Skipped {
			if s.Path == "a.fifo" {
				found = true
				if s.Reason != "non-regular-file" {
					t.Errorf("skip reason = %q, want %q", s.Reason, "non-regular-file")
				}
			}
		}
		if !found {
			t.Error("a.fifo not reported in Skipped at all")
		}
		foundReal := false
		for _, inc := range r.ctx.Included {
			if inc.Path == "real.txt" {
				foundReal = true
			}
		}
		if !foundReal {
			t.Error("real.txt (a genuine regular file alongside the FIFO) must still be included")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CollectContext hung — the FIFO was opened for read instead of skipped by dir-entry type")
	}
}
