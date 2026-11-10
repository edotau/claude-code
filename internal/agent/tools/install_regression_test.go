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
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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

// A download over maxDownload is an error, not an unbounded read.
func TestHTTPGetRefusesOversizedBody(t *testing.T) {
	orig := maxDownload
	maxDownload = 1024
	t.Cleanup(func() { maxDownload = orig })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 2048))
	}))
	t.Cleanup(srv.Close)

	if _, err := httpGet(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("2 KiB body over a 1 KiB cap: err = %v, want an exceeds error", err)
	}
}

// The installer script and `<bin> update` get no registry credential and no login, but keep proxy and CA settings.
func TestInstallChildrenScrubCredentials(t *testing.T) {
	skipWithoutPOSIXShell(t)
	setupToolsTestEnv(t)
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}}}}}`)
	gone := []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "GH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
	kept := []string{"HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE"}
	for _, k := range append(slices.Clone(gone), kept...) {
		t.Setenv(k, "SENTINEL")
	}
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "/usr/bin/env > "+filepath.Join(dir, "script")+"\n")
	}))
	t.Cleanup(srv.Close)
	if err := runInstallScript(context.Background(), io.Discard, false, srv.URL, nil, nil); err != nil {
		t.Fatalf("installer script: %v", err)
	}
	bin := filepath.Join(dir, "fakecli")
	writeToolsFixture(t, bin, "#!/bin/sh\n/usr/bin/env > "+filepath.Join(dir, "update")+"\n", 0o755)
	if err := runSelfUpdate(context.Background(), io.Discard, bin, false); err != nil {
		t.Fatalf("self update: %v", err)
	}
	for _, site := range []string{"script", "update"} {
		env, err := os.ReadFile(filepath.Join(dir, site))
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range gone {
			if strings.Contains("\n"+string(env), "\n"+k+"=") {
				t.Errorf("%s inherited %s", site, k)
			}
		}
		for _, k := range kept {
			if !strings.Contains("\n"+string(env), "\n"+k+"=SENTINEL") {
				t.Errorf("%s lost %s", site, k)
			}
		}
	}
}
