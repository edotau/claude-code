package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpencodeRefusalReportsRemovalFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires directory permissions enforced for a non-root Unix user")
	}
	setupToolsTestEnv(t)
	t.Setenv(opencodeAllowMajorEnv, "")
	dir := t.TempDir()
	src := filepath.Join(dir, "opencode")
	writeToolsFixture(t, src, "#!/bin/sh\nprintf 'opencode 2.0.0\\n'\n", 0o755)
	cause := refuseOpencodeMajor(context.Background(), io.Discard, src)
	if cause == nil {
		t.Fatal("opencode 2.0.0 was not refused")
	}
	makeToolsDirectoryReadOnly(t, dir)

	err := restoreOpencodeAside(src, src+".prev", cause)
	if !errors.Is(err, cause) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("rollback error = %v; want refusal and removal permission failure", err)
	}
	if !strings.Contains(err.Error(), "remove refused binary "+src) {
		t.Fatalf("rollback error does not identify the rejected binary: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture did not leave the refused binary installed: %v", err)
	}
}

func makeToolsDirectoryReadOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
}

func TestHTTPGetAcceptsNonAuthoritativeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNonAuthoritativeInfo)
		if _, err := io.WriteString(w, "valid download"); err != nil {
			t.Errorf("serve download: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := httpGet(context.Background(), srv.URL)
	if err != nil || string(got) != "valid download" {
		t.Fatalf("203 download = %q, %v; want valid body without error", got, err)
	}
}
