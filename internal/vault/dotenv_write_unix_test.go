//go:build linux || darwin

package vault_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

const fsizeChildEnv = "VAULT_TEST_FSIZE_CHILD"

func TestAppendDotenvRollsBackPartialWrite(t *testing.T) {
	// RLIMIT_FSIZE is process-wide and would fail go test's own testlog flush, so the limited write runs in a child.
	if os.Getenv(fsizeChildEnv) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAppendDotenvRollsBackPartialWrite$")
		cmd.Env = append(os.Environ(), fsizeChildEnv+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("limited-write child: %v\n%s", err, out)
		}
		return
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), ".env")
	const original = "KEEP=yes\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	pair := vault.Pair{Name: "TOKEN", Value: strings.Repeat("abcdef", 10)}
	n, skipped, err := limitedDotenvAppend(t, path, pair, uint64(len(original)+len("TOKEN=abc")))
	if n != 0 || len(skipped) != 0 || (!errors.Is(err, syscall.EFBIG) && !errors.Is(err, io.ErrShortWrite)) {
		t.Fatalf("limited append = %d, %v, %v; want a failed partial write", n, skipped, err)
	}
	checkDotenvContents(t, path, original)
	n, skipped, err = vault.AppendDotenv(path, []vault.Pair{pair})
	if err != nil || n != 1 || len(skipped) != 0 {
		t.Fatalf("retry = %d, %v, %v; want TOKEN written, not skipped", n, skipped, err)
	}
	checkDotenvContents(t, path, original+"TOKEN="+pair.Value+"\n")
}

func checkDotenvContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("dotenv contents = %q, want %q", got, want)
	}
}

func limitedDotenvAppend(t *testing.T, path string, pair vault.Pair, size uint64) (int, []string, error) {
	t.Helper()
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGXFSZ)
	defer signal.Stop(signals)
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
			t.Fatalf("restore file-size limit: %v", err)
		}
	}()
	limit := original
	limit.Cur = size
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	return vault.AppendDotenv(path, []vault.Pair{pair})
}
