package paths

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRTKRewrite(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "rtk")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'rtk %s' \"$2\"\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(RTKBinaryEnv, fake)
	if got := RTKRewrite(context.Background(), "git status"); got != "rtk git status" {
		t.Fatalf("present: %q", got)
	}
	if got := RTKRewrite(context.Background(), "rtk git status"); got != "rtk git status" {
		t.Fatalf("prefixed: %q", got)
	}
	t.Setenv(RTKBinaryEnv, filepath.Join(t.TempDir(), "missing"))
	if got := RTKRewrite(context.Background(), "git status"); got != "git status" {
		t.Fatalf("absent: %q", got)
	}
}
