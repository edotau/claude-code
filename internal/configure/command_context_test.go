package configure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

const configureTestToken = "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789"

func TestConfigureCommandHelper(t *testing.T) {
	if os.Getenv("CONFIGURE_COMMAND_HELPER") != "1" {
		return
	}
	idx := slices.Index(os.Args, "--")
	if idx < 0 || idx+1 == len(os.Args) {
		os.Exit(90)
	}
	args := os.Args[idx+1:]
	f, err := os.OpenFile(os.Getenv("CONFIGURE_COMMAND_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(91)
	}
	_, writeErr := fmt.Fprintln(f, strings.Join(args, "\n")+"\n")
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Exit(92)
	}
	action := args[0]
	if action == "mcp" && len(args) > 1 {
		action = args[1]
	}
	mode := os.Getenv("CONFIGURE_COMMAND_MODE")
	if mode == "cancel" || mode == "cancel-"+action {
		if err := os.WriteFile(os.Getenv("CONFIGURE_COMMAND_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(93)
		}
		time.Sleep(30 * time.Second)
		os.Exit(94)
	}
	switch action {
	case "login":
		fmt.Print("login complete\n")
	case "mint":
		fmt.Print("minting token\n" + configureTestToken + "\nfinished\n")
	}
	if mode == "fail" || mode == action+"-fail" {
		fmt.Print("command failed\n")
		os.Exit(7)
	}
	os.Exit(0)
}

func configureCommandEnv(t *testing.T, mode string) (bin, log, ready string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := t.TempDir()
	bin = pathstest.FakeVendor(t, "claude",
		`exec "$CONFIGURE_HELPER_EXE" -test.run='^TestConfigureCommandHelper$' -- "$@"`)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, ready = filepath.Join(dir, "argv"), filepath.Join(dir, "ready")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CONFIGURE_COMMAND_HELPER", "1")
	t.Setenv("CONFIGURE_HELPER_EXE", exe)
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
		want.WriteString(strings.Join(args, "\n") + "\n\n")
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
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
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

func TestLoginAndMintContext(t *testing.T) {
	for _, mint := range []bool{false, true} {
		action := "login"
		if mint {
			action = "mint"
		}
		for _, mode := range []string{"normal", "fail", "cancel"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				bin, log, ready := configureCommandEnv(t, mode)
				argv := []string{bin, action, "argument with spaces"}
				if !mint {
					argv[0] = "claude"
				}
				var token string
				run := func(ctx context.Context) error {
					if mint {
						var err error
						token, err = runMintContext(ctx, argv)
						return err
					}
					return runLoginContext(ctx, argv)
				}
				if mode == "cancel" {
					assertConfigureCancellation(t, ready, run)
					assertConfigureArgv(t, log, argv[1:])
					return
				}
				var err error
				out := captureConfigureStdout(t, func() { err = run(context.Background()) })
				if mode == "fail" {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
						t.Errorf("command error = %v, want exit status 7", err)
					}
					if token != "" {
						t.Errorf("failed mint returned token %q", token)
					}
				} else {
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
				assertConfigureArgv(t, log, argv[1:])
			})
		}
	}
}

func TestVendorArgv(t *testing.T) {
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "gemini", "agy"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			bin := pathstest.FakeVendor(t, name, "exit 0")
			argv := []string{name, "login", "argument with spaces"}
			original := slices.Clone(argv)
			got, err := cliArgv(argv)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{bin, "login", "argument with spaces"}; !slices.Equal(got, want) {
				t.Errorf("vendorArgv = %v, want %v", got, want)
			}
			if !slices.Equal(argv, original) {
				t.Errorf("input changed to %v, want %v", argv, original)
			}
			got[1] = "changed"
			if !slices.Equal(argv, original) {
				t.Errorf("result aliases input: %v", argv)
			}
		})
	}
}

func TestVendorArgvGithubUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	argv := []string{"gh", "auth", "login"}
	got, err := cliArgv(argv)
	if err != nil || !slices.Equal(got, argv) {
		t.Errorf("vendorArgv = %v, %v; want %v, nil", got, err, argv)
	}
}

func TestVendorArgvMissingVendor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	argv := []string{"copilot", "login"}
	original := slices.Clone(argv)
	_, wantErr := paths.VendorBin("copilot")
	if wantErr == nil {
		t.Fatal("expected missing vendor")
	}
	got, err := cliArgv(argv)
	if got != nil || err == nil || err.Error() != wantErr.Error() {
		t.Errorf("vendorArgv = %v, %v; want nil, %v", got, err, wantErr)
	}
	if !slices.Equal(argv, original) {
		t.Errorf("input changed to %v, want %v", argv, original)
	}
}

func TestRegisterClaudeMCPContext(t *testing.T) {
	remove := []string{"mcp", "remove", "-s", "user", "web_search"}
	add := []string{"mcp", "add", "-s", "user", "web_search", "--", "/custom path/claude-code", "network", "websearch"}
	for _, mode := range []string{"normal", "remove-fail", "add-fail", "cancel-remove", "cancel-add"} {
		t.Run(mode, func(t *testing.T) {
			_, log, ready := configureCommandEnv(t, mode)
			run := func(ctx context.Context) error {
				return registerClaudeMCPContext(ctx, "/custom path/claude-code")
			}
			if strings.HasPrefix(mode, "cancel-") {
				assertConfigureCancellation(t, ready, run)
			} else {
				err := run(context.Background())
				if mode == "add-fail" {
					if err == nil || !strings.Contains(err.Error(), "claude mcp add") ||
						!strings.Contains(err.Error(), "command failed") {
						t.Errorf("add failure = %v, want command and output detail", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancel-remove" {
				assertConfigureArgv(t, log, remove)
			} else {
				assertConfigureArgv(t, log, remove, add)
			}
		})
	}
}
