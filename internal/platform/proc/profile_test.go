package proc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteProfilesCreatesHeapAndCPU(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profiles")
	if err := writeProfiles(dir, 50*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"heap", "cpu"} {
		m, _ := filepath.Glob(filepath.Join(dir, "*."+kind+".pprof"))
		if len(m) != 1 {
			t.Fatalf("%s profiles = %v", kind, m)
		}
		if fi, err := os.Stat(m[0]); err != nil || fi.Size() == 0 {
			t.Fatalf("%s profile empty: %v", kind, err)
		}
	}
}

func TestWriteProfilesStopsEarlyAndSkipsBusyCPU(t *testing.T) {
	done := make(chan struct{})
	close(done)
	dir := t.TempDir()
	start := time.Now()
	if err := writeProfiles(dir, time.Minute, done); err != nil || time.Since(start) > 10*time.Second {
		t.Fatalf("early stop: err=%v after %v", err, time.Since(start))
	}
}
