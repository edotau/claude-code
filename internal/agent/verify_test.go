package agent

import (
	"context"
	"os"
	"testing"
)

func TestRunVerifyRecordsExit(t *testing.T) {
	ok := runVerify(context.Background(), t.TempDir(), "go vet ./nonexistentpkg", os.Environ())
	if ok.Exit == 0 || ok.Tail == "" || ok.Command != "go vet ./nonexistentpkg" {
		t.Fatalf("failed step must record a non-zero exit and output: %+v", ok)
	}
	if bad := runVerify(context.Background(), "", "rm -rf x", nil); bad.Exit != -1 || bad.Reason == "" {
		t.Fatalf("a disallowed shape must not run: %+v", bad)
	}
}
