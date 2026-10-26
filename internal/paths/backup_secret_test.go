package paths

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// WriteBackup numbers collisions base, base-1, base-2 and never overwrites; files are 0600.
func TestWriteBackupNumbersCollisions(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cfg.bak")
	for i, want := range []string{base, base + "-1", base + "-2"} {
		got, err := WriteBackup(base, []byte{byte('a' + i)})
		if err != nil || got != want {
			t.Fatalf("call %d: got %q, %v; want %q", i, got, err, want)
		}
		b, _ := os.ReadFile(got)
		if string(b) != string([]byte{byte('a' + i)}) {
			t.Fatalf("%s content %q", got, b)
		}
		if fi, _ := os.Stat(got); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", got, fi.Mode().Perm())
		}
	}
}

// ClientSecret mints 64 hex chars once, stores them 0600, and returns the same value afterwards.
func TestClientSecretMintsOnceAndReuses(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	a, err := ClientSecret(file)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a) {
		t.Fatalf("secret %q, %v", a, err)
	}
	b, err := ClientSecret(file)
	if err != nil || a != b {
		t.Fatalf("second call %q, %v; want %q", b, err, a)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// An empty secret file is re-minted rather than returned.
func TestClientSecretReplacesEmptyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ClientSecret(file)
	if err != nil || len(s) != 64 {
		t.Fatalf("secret %q, %v", s, err)
	}
}
