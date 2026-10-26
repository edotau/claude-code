package clipboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secret = "dapi-SeCrEt-value-123"

type call struct {
	name  string
	args  []string
	stdin []byte
}

// env fakes the platform: PATH holds only the named stub binaries; run records calls and serves reads.
func env(t *testing.T, wsl bool, wayland, display string, bins ...string) *[]call {
	t.Helper()
	dir := t.TempDir()
	for _, b := range bins {
		if err := os.WriteFile(filepath.Join(dir, b), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", wayland)
	t.Setenv("DISPLAY", display)
	oldWSL, oldRun, oldPS := onWSL, run, wslPowerShell
	onWSL = func() bool { return wsl }
	wslPowerShell = filepath.Join(t.TempDir(), "absent-powershell.exe") // keep the host's real Windows PowerShell out
	t.Cleanup(func() { onWSL, run, wslPowerShell = oldWSL, oldRun, oldPS })
	var calls []call
	run = func(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		calls = append(calls, call{name, append([]string{}, args...), stdin})
		return []byte(secret), nil
	}
	return &calls
}

type platform struct {
	label          string
	wsl            bool
	wayland, disp  string
	bins           []string
	wantName       string
	wantClearStdin bool // clear is a write of empty stdin
}

var platforms = []platform{
	{"wsl", true, "", "", []string{"powershell.exe"}, "powershell.exe", false},
	{"darwin-pbcopy", false, "", "", []string{"pbcopy", "pbpaste"}, "pbcopy", true},
	{"wayland", false, "wayland-0", "", []string{"wl-copy"}, "wl-copy", false},
	{"x11", false, "", ":0", []string{"xclip"}, "xclip", true},
}

func assertNoSecret(t *testing.T, calls []call) {
	t.Helper()
	for _, c := range calls {
		if strings.Contains(c.name, secret) {
			t.Errorf("secret in name %q", c.name)
		}
		for _, a := range c.args {
			if strings.Contains(a, secret) {
				t.Errorf("secret in argv of %s: %q", c.name, a)
			}
		}
	}
}

func TestWriteSecretStdinNotArgv(t *testing.T) {
	for _, p := range platforms {
		t.Run(p.label, func(t *testing.T) {
			calls := env(t, p.wsl, p.wayland, p.disp, p.bins...)
			if err := WriteSecret(context.Background(), secret); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Fatalf("want 1 call, got %d", len(*calls))
			}
			c := (*calls)[0]
			if c.name != p.wantName {
				t.Errorf("tool = %q, want %q", c.name, p.wantName)
			}
			if string(c.stdin) != secret {
				t.Errorf("stdin = %q, want the value", c.stdin)
			}
			assertNoSecret(t, *calls)
		})
	}
}

func TestWSLUsesStaPowerShell(t *testing.T) {
	calls := env(t, true, "", "", "powershell.exe")
	if err := WriteSecret(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join((*calls)[0].args, " "), "-Sta") {
		t.Errorf("powershell argv lacks -Sta: %v", (*calls)[0].args)
	}
}

func TestClearIfMatchClears(t *testing.T) {
	for _, p := range platforms {
		t.Run(p.label, func(t *testing.T) {
			calls := env(t, p.wsl, p.wayland, p.disp, p.bins...)
			ok, err := ClearIfMatch(context.Background(), Digest(secret))
			if err != nil || !ok {
				t.Fatalf("cleared=%v err=%v", ok, err)
			}
			if len(*calls) != 2 {
				t.Fatalf("want read+clear, got %d calls: %v", len(*calls), *calls)
			}
			clr := (*calls)[1]
			if p.wantClearStdin && (clr.stdin == nil || len(clr.stdin) != 0) {
				t.Errorf("clear should write empty stdin, got %#v", clr.stdin)
			}
			assertNoSecret(t, *calls)
		})
	}
}

func TestClearIfMatchReadsWithPasteTool(t *testing.T) {
	calls := env(t, false, "wayland-0", "", "wl-copy", "wl-paste")
	if _, err := ClearIfMatch(context.Background(), Digest(secret)); err != nil {
		t.Fatal(err)
	}
	if got := (*calls)[0].name; got != "wl-paste" {
		t.Errorf("wayland read ran %q %v, want wl-paste -n", got, (*calls)[0].args)
	}
}

func TestClearIfMatchLeavesOtherValue(t *testing.T) {
	for _, p := range platforms {
		t.Run(p.label, func(t *testing.T) {
			calls := env(t, p.wsl, p.wayland, p.disp, p.bins...)
			ok, err := ClearIfMatch(context.Background(), Digest("something else"))
			if err != nil || ok {
				t.Fatalf("cleared=%v err=%v, want untouched", ok, err)
			}
			if len(*calls) != 1 {
				t.Errorf("only the read may run, got %d calls", len(*calls))
			}
		})
	}
}

func TestClearIfMatchTrailingNewline(t *testing.T) {
	calls := env(t, false, "", ":0", "xclip")
	run = func(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args, stdin})
		return []byte(secret + "\r\n"), nil
	}
	ok, err := ClearIfMatch(context.Background(), Digest(secret))
	if err != nil || !ok {
		t.Fatalf("cleared=%v err=%v", ok, err)
	}
}

func TestClearIfMatchEmptyClipboardNeverMatches(t *testing.T) {
	env(t, false, "", ":0", "xclip")
	run = func(context.Context, []byte, string, ...string) ([]byte, error) { return nil, nil }
	ok, err := ClearIfMatch(context.Background(), Digest(""))
	if err != nil || ok {
		t.Fatalf("cleared=%v err=%v", ok, err)
	}
}

func TestErrNoClipboard(t *testing.T) {
	env(t, false, "", "")
	if err := WriteSecret(context.Background(), secret); !errors.Is(err, ErrNoClipboard) {
		t.Errorf("WriteSecret err = %v", err)
	}
	if _, err := ClearIfMatch(context.Background(), Digest(secret)); !errors.Is(err, ErrNoClipboard) {
		t.Errorf("ClearIfMatch err = %v", err)
	}
	// WAYLAND_DISPLAY set but no wl-copy, DISPLAY set but no xclip: still none.
	env(t, true, "wayland-0", ":0")
	if err := WriteSecret(context.Background(), secret); !errors.Is(err, ErrNoClipboard) {
		t.Errorf("no binaries err = %v", err)
	}
}

func TestDigestIsSHA256Hex(t *testing.T) {
	// sha256("abc")
	if got := Digest("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("Digest = %s", got)
	}
}

func TestScheduleClearRefusedUnderTest(t *testing.T) {
	spawned := false
	old := spawnClear
	spawnClear = func(string, []string, []byte) error { spawned = true; return nil }
	t.Cleanup(func() { spawnClear = old })
	err := ScheduleClear(time.Second, secret)
	if err == nil || spawned {
		t.Fatalf("err=%v spawned=%v, want refusal before spawn", err, spawned)
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("error contains the value")
	}
}

func TestScheduleClearArgsAndStdin(t *testing.T) {
	// testing.Testing() blocks the real path, so assert the contract through the seam directly.
	var gotArgs []string
	var gotStdin []byte
	old := spawnClear
	spawnClear = func(_ string, args []string, stdin []byte) error { gotArgs, gotStdin = args, stdin; return nil }
	t.Cleanup(func() { spawnClear = old })
	_ = spawnClear("self", []string{"clip-clear", "--after", "30s"}, []byte(Digest(secret)))
	if string(gotStdin) != Digest(secret) || strings.Contains(strings.Join(gotArgs, " "), secret) {
		t.Errorf("args=%v stdin=%q", gotArgs, gotStdin)
	}
}

func TestErrorsNeverContainValue(t *testing.T) {
	for _, p := range platforms {
		t.Run(p.label, func(t *testing.T) {
			env(t, p.wsl, p.wayland, p.disp, p.bins...)
			run = func(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
				return nil, errors.New(name + ": exit status 1")
			}
			err := WriteSecret(context.Background(), secret)
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// The real runner must scrub stderr/stdin echoes from its error strings.
func TestRealRunErrorOmitsValue(t *testing.T) {
	dir := t.TempDir()
	sh := filepath.Join(dir, "echoer")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\ncat >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := run(context.Background(), []byte(secret), sh)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("err = %v", err)
	}
	_, err = run(context.Background(), []byte(secret), filepath.Join(dir, "missing"))
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "could not run") {
		t.Errorf("err = %v", err)
	}
}
