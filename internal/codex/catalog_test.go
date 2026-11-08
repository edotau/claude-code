package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCodexCatalogWrapsExitErrorWithTrimmedStderr(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '  bad catalog  \\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runCodexCatalog(context.Background(), bin, home, 5*time.Second, "debug", "models", "--bundled")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("expected wrapped exit status 1, got %v", err)
	}
	if got, want := err.Error(), "exit status 1: bad catalog"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestRunCodexCatalogErrorCarriesStderr(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'bad model_catalog_json at line 3' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runCodexCatalog(context.Background(), bin, t.TempDir(), 5*time.Second, "debug", "models")
	if err == nil || !strings.Contains(err.Error(), "bad model_catalog_json at line 3") {
		t.Fatalf("err = %v, want codex's stderr in it", err)
	}
}
