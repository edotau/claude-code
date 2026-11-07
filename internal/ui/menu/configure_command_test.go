package menu

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

const configureTestToken = "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789"

// configureFakeClaude logs argv, then acts on CONFIGURE_COMMAND_MODE (exists: the first add reports a duplicate until remove runs); `exec sleep` leaves SIGINT nothing to defer.
const configureFakeClaude = `printf '%s\n' "$@" '' >>"$CONFIGURE_COMMAND_LOG"
action=$1
if [ "$1" = mcp ] && [ $# -gt 1 ]; then action=$2; fi
mode=$CONFIGURE_COMMAND_MODE
if [ "$mode" = cancel ] || [ "$mode" = "cancel-$action" ]; then
	: >"$CONFIGURE_COMMAND_READY"
	exec "$CONFIGURE_SLEEP" 30
fi
case $action in
login) echo 'login complete' ;;
mint) printf 'minting token\n` + configureTestToken + `\nfinished\n' ;;
esac
case $mode in exists|exists-remove-fail)
	if [ "$action" = remove ]; then
		: >"$CONFIGURE_COMMAND_LOG.removed"
		if [ "$mode" = exists-remove-fail ]; then echo 'remove failed'; exit 7; fi
	fi
	if [ "$action" = add ] && [ ! -e "$CONFIGURE_COMMAND_LOG.removed" ]; then
		echo 'MCP server web_search already exists'
		exit 1
	fi ;;
esac
if [ "$mode" = fail ] || [ "$mode" = "$action-fail" ]; then
	echo 'command failed'
	exit 7
fi`

func configureCommandEnv(t *testing.T, mode string) (bin, log, ready string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := t.TempDir()
	bin = pathstest.FakeVendor(t, "claude", configureFakeClaude)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	log, ready = filepath.Join(dir, "argv"), filepath.Join(dir, "ready")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CONFIGURE_SLEEP", sleep)
	t.Setenv("CONFIGURE_COMMAND_MODE", mode)
	t.Setenv("CONFIGURE_COMMAND_LOG", log)
	t.Setenv("CONFIGURE_COMMAND_READY", ready)
	orig := actionConfirmed
	actionConfirmed = func(string) bool { return true }
	t.Cleanup(func() { actionConfirmed = orig })
	return bin, log, ready
}

func captureConfigureStdout(t *testing.T, run func()) string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close stdout capture: %v", err)
		}
	})
	orig := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = orig }()
	run()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func assertConfigureArgv(t *testing.T, log string, commands ...[]string) {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, args := range commands {
		want.WriteString(strings.Join(args, "\n"))
		want.WriteString("\n\n")
	}
	if string(raw) != want.String() {
		t.Errorf("command argv = %q, want %q", raw, want.String())
	}
}

func assertConfigureCancellation(t *testing.T, ready string, run func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	waitConfigureReady(t, ctx, ready, done)
	canceledAt := time.Now()
	cancel()
	select {
	case err := <-done:
		// under WaitDelay (3s): SIGINT ended the child, not the kill fallback
		if d := time.Since(canceledAt); d > 2500*time.Millisecond {
			t.Errorf("cancellation took %v, want prompt SIGINT exit (< 2.5s, far under the 30s helper sleep)", d)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled command error = %v, want wrapped context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not return promptly after cancellation")
	}
}

// waitConfigureReady polls until the helper creates ready, failing if the command ends or ctx expires first.
func waitConfigureReady(t *testing.T, ctx context.Context, ready string, done <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(ready)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("command ended before cancellation: %v", err)
		case <-ctx.Done():
			t.Fatal("helper did not become ready before the deadline")
		case <-ticker.C:
		}
	}
}

func TestLoginAndMintContext(t *testing.T) {
	for _, mint := range []bool{false, true} {
		action := "login"
		if mint {
			action = "mint"
		}
		for _, mode := range []string{"normal", "fail", "cancel"} {
			t.Run(action+"/"+mode, func(t *testing.T) { runLoginMintCase(t, action, mode) })
		}
	}
}

// runLoginMintCase runs the fake claude's login or mint action in mode (normal, fail, cancel) and checks the outcome.
func runLoginMintCase(t *testing.T, action, mode string) {
	mint := action == "mint"
	bin, log, ready := configureCommandEnv(t, mode)
	argv := []string{bin, action, "argument with spaces"}
	if !mint {
		argv[0] = "claude"
	}
	var token string
	run := func(ctx context.Context) error {
		if !mint {
			return configure.RunLoginContext(ctx, argv)
		}
		var err error
		token, err = configure.RunMintContext(ctx, argv)
		return err
	}
	if mode == "cancel" {
		assertConfigureCancellation(t, ready, run)
		assertConfigureArgv(t, log, argv[1:])
		return
	}
	var err error
	out := captureConfigureStdout(t, func() { err = run(context.Background()) })
	if mode == "fail" {
		assertCommandFailed(t, err, token)
	} else {
		assertCommandSucceeded(t, err, out, token, mint)
	}
	assertConfigureArgv(t, log, argv[1:])
}

// assertCommandFailed requires the fake command's exit status 7 and no minted token.
func assertCommandFailed(t *testing.T, err error, token string) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Errorf("command error = %v, want exit status 7", err)
	}
	if token != "" {
		t.Errorf("failed mint returned token %q", token)
	}
}

// assertCommandSucceeded requires a clean run whose stdout (and, for a mint, token) match the fake's script.
func assertCommandSucceeded(t *testing.T, err error, out, token string, mint bool) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	wantOut := "login complete\n"
	if mint {
		wantOut = "minting token\n" + configureTestToken + "\nfinished\n"
		if token != configureTestToken {
			t.Errorf("minted token = %q, want %q", token, configureTestToken)
		}
	}
	if out != wantOut {
		t.Errorf("stdout = %q, want %q", out, wantOut)
	}
}
