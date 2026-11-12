package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// digestFixture is a dir with one raw archive file, a prior digest.md, and a fake claude script body.
func digestFixture(t *testing.T, script string) (dir, fake, digest string) {
	t.Helper()
	dir = t.TempDir()
	arch := filepath.Join(dir, "archive")
	if err := os.MkdirAll(arch, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(arch, "sessionHistory-2026-10.md"), "- 2026-10-01 did a thing\n")
	digest = filepath.Join(arch, digestFile)
	write(t, digest, "PREVIOUS\n")
	fake = filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, fake, digest
}

func TestDigestRunsWithoutToolsOrSettings(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	dir, fake, _ := digestFixture(t, `for a in "$@"; do printf '[%s]\n' "$a"; done > '`+argvFile+"'\nexit 1\n")
	if _, err := Digest(context.Background(), dir, fake); err == nil {
		t.Fatal("want the fake's exit 1 surfaced")
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{"[--tools]\n[]\n", "[--setting-sources]\n[]\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}
}

func TestDigestCommitFailureRestoresPreviousDigest(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	dir := HomeBankDir()
	archive := archiveDir(dir)
	write(t, filepath.Join(archive, "sessionHistory-2026-10.md"), "- a durable archived fact\n")
	digest := filepath.Join(archive, digestFile)
	write(t, digest, "PREVIOUS\n")
	write(t, filepath.Join(StoreDir(), ".git", "index.lock"), "held by another writer\n")
	fake := filepath.Join(t.TempDir(), "claude")
	write(t, fake, "#!/bin/sh\nprintf '%s\\n' '- replacement digest'\n")
	if err := os.Chmod(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Digest(context.Background(), dir, fake)
	if err == nil || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("want commit failure from held index.lock, got %v", err)
	}
	if got := mustRead(t, digest); got != "PREVIOUS\n" {
		t.Fatalf("failed commit lost previous digest: %q", got)
	}
}

func TestDigestCancelStopsChildKeepsPreviousDigest(t *testing.T) {
	dir, fake, digest := digestFixture(t, "exec sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := Digest(ctx, dir, fake)
	if err == nil {
		t.Fatal("want an error after cancel")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("cancel took %v, want prompt", d)
	}
	if b, _ := os.ReadFile(digest); string(b) != "PREVIOUS\n" {
		t.Errorf("digest changed on cancel: %q", b)
	}
}
