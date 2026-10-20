package keyring

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLookupToolFailureIsUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires a supported keyring CLI")
	}
	missCode := "1"
	if runtime.GOOS == "darwin" {
		missCode = "44"
	}
	for _, tc := range []struct {
		name, script string
		value        string
		want         error
	}{
		{"found", "printf '%s\\n' representative-key", "representative-key", nil},
		{"absent", "exit " + missCode, "", nil},
		{"stderr_failure", "echo 'fake keyring unavailable' >&2\nexit 1", "", ErrUnavailable},
		{"other_exit", "exit 2", "", ErrUnavailable},
		{"hanging", "exec /bin/sleep 2", "", ErrNoAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, Tool()), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			saved := timeout
			timeout = 50 * time.Millisecond
			t.Cleanup(func() { timeout = saved })

			value, found, err := Lookup("PHASE1B")
			if value != tc.value || found != (tc.value != "") || !errors.Is(err, tc.want) {
				t.Fatalf("Lookup = (%q, %t, %v), want (%q, %t, %v)",
					value, found, err, tc.value, tc.value != "", tc.want)
			}
		})
	}
}

func TestLookupNoToolIsUnavailable(t *testing.T) {
	hermetic(t)
	value, found, err := Lookup("PHASE1B")
	if value != "" || found || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Lookup without tool = (%q, %t, %v), want empty, false, ErrUnavailable", value, found, err)
	}
}
