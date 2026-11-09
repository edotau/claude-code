package tools

import (
	"bytes"
	"compress/gzip"
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

	"github.com/edotau/claude-code/internal/paths"
)

func TestLinkRelativePathEntryRemainsUsable(t *testing.T) {
	setupToolsTestEnv(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "vendor", "bin", "codex")
	writeToolsFixture(t, candidate, "#!/bin/sh\nexit 0\n", 0o755)
	t.Chdir(root)
	setToolsPath(t, filepath.Join("vendor", "bin"))

	Link(io.Discard, []string{"codex"}, false)
	dst := filepath.Join(paths.BinDir(), "codex")
	if got := readlink(dst); got != candidate {
		t.Fatalf("relative PATH link target = %q, want absolute %q", got, candidate)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("installed link is unusable: %v", err)
	}
}

func TestExtractTarGzRejectsCorruptChecksum(t *testing.T) {
	data := buildTarGz(t, "codex", "corrupted executable bytes")
	data[len(data)-8] ^= 0xff

	got, err := extractTarGzSingleFile(data)
	if !errors.Is(err, gzip.ErrChecksum) {
		t.Fatalf("corrupt archive extracted %q with error %v; want gzip checksum failure", got, err)
	}
	if got != nil {
		t.Fatalf("checksum failure returned executable bytes: %q", got)
	}
}

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

func TestScienceRollbackFailureStopsFallback(t *testing.T) {
	setupToolsTestEnv(t)
	stubScienceTools(t, scienceTeamID)
	base, fallback := t.TempDir(), t.TempDir()
	prior := filepath.Join(base, scienceAppName)
	writeToolsFixture(t, filepath.Join(prior, "prior"), "original app", 0o644)
	root := filepath.Join(t.TempDir(), "Applications")
	// Moving the old app invalidates this alias, so both the swap and restore fail without races.
	symlinkToolsFixture(t, prior+string(os.PathSeparator)+"..", root)
	oldRoots := scienceAppRoots
	scienceAppRoots = func() []string { return []string{root, fallback} }
	t.Cleanup(func() { scienceAppRoots = oldRoots })

	var out bytes.Buffer
	dest, err := installAppFromDMG(context.Background(), &out, "fixture.dmg", t.TempDir())
	if dest != "" || !errors.Is(err, errAppRollback) {
		t.Fatalf("install returned %q, %v; want rollback failure instead of fallback success; output: %s", dest, err, &out)
	}
	if _, statErr := os.Stat(filepath.Join(fallback, scienceAppName)); !os.IsNotExist(statErr) {
		t.Fatalf("fallback was attempted after failed rollback: %v", statErr)
	}
	assertSciencePreviousAppPreserved(t, base, err)
}

func assertSciencePreviousAppPreserved(t *testing.T, base string, installErr error) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".claude-science-") {
		t.Fatalf("expected one preserved staging directory, got %v", entries)
	}
	old := filepath.Join(entries[0].Name(), "old.app")
	got, err := os.ReadFile(filepath.Join(base, old, "prior"))
	if err != nil || string(got) != "original app" {
		t.Fatalf("displaced app = %q, %v; want original app preserved", got, err)
	}
	if !strings.Contains(installErr.Error(), old) {
		t.Fatalf("rollback error does not identify displaced app %q: %v", old, installErr)
	}
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
