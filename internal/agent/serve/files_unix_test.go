package serve

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReadStateRejectsUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file guards")
	}
	for _, kind := range []string{"symlink", "fifo", "public", "oversized", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if err := ensureDir(Dir()); err != nil {
				t.Fatal(err)
			}
			plantServeFile(t, stateFile(), kind)
			done := make(chan error, 1)
			go func() { _, err := ReadState(); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unsafe state accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("unsafe state blocked ReadState")
			}
		})
	}
}

func plantServeFile(t *testing.T, path, kind string) {
	t.Helper()
	data := []byte(`{"schema":1,"pid":123}`)
	var err error
	switch kind {
	case "symlink", "hardlink":
		target := filepath.Join(t.TempDir(), "target")
		if err = os.WriteFile(target, data, 0o600); err == nil {
			if kind == "symlink" {
				err = os.Symlink(target, path)
			} else {
				err = os.Link(target, path)
			}
		}
	case "fifo":
		err = exec.Command("/usr/bin/mkfifo", "-m", "600", path).Run()
	case "public":
		if err = os.WriteFile(path, data, 0o600); err == nil {
			err = os.Chmod(path, 0o644)
		}
	case "oversized":
		err = os.WriteFile(path, append(data, bytes.Repeat([]byte(" "), maxStateBytes)...), 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestServeLocksRejectUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file guards")
	}
	for _, name := range []string{"spawn.lock", "serve.json.lock"} {
		for _, kind := range []string{"symlink", "fifo"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
				if err := ensureDir(Dir()); err != nil {
					t.Fatal(err)
				}
				plantServeFile(t, filepath.Join(Dir(), name), kind)
				err := withServeLock(name, func() error { t.Error("unsafe lock acquired"); return nil })
				if err == nil {
					t.Fatal("unsafe lock accepted")
				}
			})
		}
	}
}

func TestServeFilesRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file guards")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := ensureDir(Dir()); err != nil {
		t.Fatal(err)
	}
	want := State{PID: os.Getpid(), Started: time.Now().UTC(), Cap: 2}
	cleanup, err := writeState(want)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got, err := ReadState(); err != nil || got.PID != want.PID || !got.Started.Equal(want.Started) {
		t.Fatalf("state round trip = %+v, %v", got, err)
	}
	f, err := openLog()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("daemon log\n"); err != nil {
		t.Fatal(err)
	}
}
