package pathstest

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestFakeVendorRejectsExternalSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		executable bool
		want       string
	}{
		{name: "bin", want: "bin dir"},
		{name: "bin/codex", executable: true, want: "refusing to overwrite symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			external := externalVendorSymlink(t, tc.name, tc.executable)
			expectVendorFatal(t, tc.want, func(tb testing.TB) {
				FakeVendor(tb, "codex", "echo fake")
			})
			got, err := os.ReadFile(external)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "original external executable\n" {
				t.Errorf("external executable was overwritten: %q", got)
			}
		})
	}
}

func externalVendorSymlink(t *testing.T, link string, executable bool) string {
	t.Helper()
	cfg, outside := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	external := filepath.Join(outside, "codex")
	if err := os.WriteFile(external, []byte("original external executable\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := outside
	if executable {
		target = external
	}
	link = filepath.Join(cfg, filepath.FromSlash(link))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return external
}

type vendorFatal string

type fatalVendorTB struct {
	testing.TB
}

func (tb fatalVendorTB) Fatalf(format string, args ...any) {
	panic(vendorFatal(fmt.Sprintf(format, args...)))
}

func expectVendorFatal(t *testing.T, want string, run func(testing.TB)) {
	t.Helper()
	defer func() {
		got := recover()
		message, ok := got.(vendorFatal)
		if !ok {
			t.Errorf("expected FakeVendor refusal containing %q, got %v", want, got)
			return
		}
		if !strings.Contains(string(message), want) {
			t.Errorf("FakeVendor refusal = %q, want %q", message, want)
		}
	}()
	run(fatalVendorTB{TB: t})
}

func TestFakeVendorRefusesRealConfig(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", u.HomeDir)

	tb := &fatalTB{TB: t}
	returned := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		FakeVendor(tb, "codex", "exit 0")
		returned <- struct{}{}
	}()
	<-done

	if !tb.called {
		t.Fatal("FakeVendor did not call Fatal")
	}
	for _, fragment := range []string{"refusing to write", "real ~/.claude"} {
		if !strings.Contains(tb.message, fragment) {
			t.Errorf("Fatal message %q does not contain %q", tb.message, fragment)
		}
	}
	select {
	case <-returned:
		t.Fatal("FakeVendor returned instead of stopping before writing")
	default:
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got, want := FakeVendor(t, "codex", "exit 0"), filepath.Join(paths.BinDir(), "codex"); got != want {
		t.Fatalf("FakeVendor path = %q, want %q", got, want)
	}
}

type fatalTB struct {
	testing.TB
	called  bool
	message string
}

func (tb *fatalTB) Fatal(args ...any) {
	tb.called = true
	tb.message = fmt.Sprint(args...)
	runtime.Goexit()
}

func (tb *fatalTB) Fatalf(format string, args ...any) {
	tb.called = true
	tb.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
