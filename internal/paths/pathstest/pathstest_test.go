package pathstest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
