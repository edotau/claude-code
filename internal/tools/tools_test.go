package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func toolsTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".tools-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func writeToolsFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func symlinkToolsFixture(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func npmToolsFixture(t *testing.T, root, name, pkg string) string {
	t.Helper()
	prefix := filepath.Join(root, "npm prefix")
	target := filepath.Join(prefix, "lib", "node_modules", pkg, "bin", name)
	writeToolsFixture(t, target, "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	symlinkToolsFixture(t, target, filepath.Join(prefix, "bin", name))
	return prefix
}

func isolateToolsEnv(t *testing.T, root string) (string, string) {
	t.Helper()
	home := filepath.Join(root, "home")
	bin := filepath.Join(home, ".local", "bin")
	marker := filepath.Join(root, "npm-called")
	writeToolsFixture(t, filepath.Join(bin, "npm"),
		"#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+strconv.Quote(marker)+"\n", 0o755)
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	return bin, marker
}

func stubToolsInstall(t *testing.T, name string, install action) {
	t.Helper()
	old := Tools
	Tools = append([]Tool(nil), Tools...)
	t.Cleanup(func() { Tools = old })
	for i := range Tools {
		if Tools[i].Name == name {
			Tools[i].Install = install
			Tools[i].Update = nil
			return
		}
	}
	t.Fatalf("tool %q is missing", name)
}

func assertNpmNotRun(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("npm must not execute: marker stat error = %v", err)
	}
}

func npmCleanupLines(output string) []string {
	var lines []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "  npm      uninstall ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestInstallSelectedDryRunCleansEveryNpmPrefix(t *testing.T) {
	for _, tc := range []struct{ name, pkg string }{
		{"opencode", "opencode-ai"},
		{"codex", "@openai/codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := toolsTestDir(t)
			localBin, marker := isolateToolsEnv(t, root)
			writeToolsFixture(t, filepath.Join(localBin, tc.name), "#!/bin/sh\nexit 0\n", 0o755)
			prefixA := npmToolsFixture(t, filepath.Join(root, "a"), tc.name, tc.pkg)
			prefixB := npmToolsFixture(t, filepath.Join(root, "b"), tc.name, tc.pkg)
			alias := filepath.Join(root, "alias-bin")
			symlinkToolsFixture(t, filepath.Join(prefixA, "bin"), alias)

			missing := filepath.Join(root, "missing", "lib", "node_modules", tc.pkg, "bin", tc.name)
			missingBin := filepath.Join(root, "missing", "bin")
			symlinkToolsFixture(t, missing, filepath.Join(missingBin, tc.name))
			nonexecPrefix := npmToolsFixture(t, filepath.Join(root, "nonexec"), tc.name, tc.pkg)
			if err := os.Chmod(filepath.Join(nonexecPrefix, "lib", "node_modules", tc.pkg, "bin", tc.name), 0o644); err != nil {
				t.Fatal(err)
			}
			dirPrefix := filepath.Join(root, "directory")
			dirTarget := filepath.Join(dirPrefix, "lib", "node_modules", tc.pkg, "bin", tc.name)
			if err := os.MkdirAll(dirTarget, 0o755); err != nil {
				t.Fatal(err)
			}
			symlinkToolsFixture(t, dirTarget, filepath.Join(dirPrefix, "bin", tc.name))
			t.Setenv("PATH", strings.Join([]string{
				localBin, missingBin, filepath.Join(nonexecPrefix, "bin"), filepath.Join(dirPrefix, "bin"),
				filepath.Join(prefixA, "bin"), alias, filepath.Join(prefixA, "bin"), filepath.Join(prefixB, "bin"),
			}, string(os.PathListSeparator)))

			if tc.name == "codex" {
				calls := 0
				stubToolsInstall(t, tc.name, func(_ context.Context, _ io.Writer, dryRun bool) error {
					calls++
					if !dryRun {
						t.Error("expected a dry-run installer")
					}
					return nil
				})
				t.Cleanup(func() {
					if calls != 1 {
						t.Errorf("installer calls = %d, want 1", calls)
					}
				})
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			if err := InstallSelected(ctx, &out, []string{tc.name}, true); err != nil {
				t.Fatal(err)
			}
			want := []string{
				"  npm      uninstall -g --prefix " + prefixA + " " + tc.pkg,
				"  npm      uninstall -g --prefix " + prefixB + " " + tc.pkg,
			}
			if got := npmCleanupLines(out.String()); !reflect.DeepEqual(got, want) {
				t.Errorf("cleanup lines = %q, want %q\noutput:\n%s", got, want, out.String())
			}
			assertNpmNotRun(t, marker)
		})
	}
}

func TestInstallSelectedFailureSkipsNpmCleanup(t *testing.T) {
	root := toolsTestDir(t)
	localBin, marker := isolateToolsEnv(t, root)
	writeToolsFixture(t, filepath.Join(localBin, "opencode"), "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	prefix := npmToolsFixture(t, root, "opencode", "opencode-ai")
	t.Setenv("PATH", localBin+string(os.PathListSeparator)+filepath.Join(prefix, "bin"))
	wantErr := errors.New("installer failed")
	stubToolsInstall(t, "opencode", func(context.Context, io.Writer, bool) error { return wantErr })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := InstallSelected(ctx, &out, []string{"opencode"}, false); !errors.Is(err, wantErr) {
		t.Fatalf("InstallSelected error = %v, want %v", err, wantErr)
	}
	if got := npmCleanupLines(out.String()); len(got) != 0 {
		t.Errorf("failed install planned cleanup: %q", got)
	}
	assertNpmNotRun(t, marker)
}

func TestInstallSelectedKeepsNpmWithoutWorkingLocalVersion(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"missing", ""},
		{"failed-version", "#!/bin/sh\nprintf '1.0.0\\n'\nexit 1\n"},
		{"empty-version", "#!/bin/sh\nexit 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := toolsTestDir(t)
			localBin, marker := isolateToolsEnv(t, root)
			if tc.script != "" {
				writeToolsFixture(t, filepath.Join(localBin, "opencode"), tc.script, 0o755)
			}
			prefix := npmToolsFixture(t, root, "opencode", "opencode-ai")
			t.Setenv("PATH", localBin+string(os.PathListSeparator)+filepath.Join(prefix, "bin"))
			stubToolsInstall(t, "opencode", func(_ context.Context, _ io.Writer, dryRun bool) error {
				if dryRun {
					t.Error("expected a real-run version guard")
				}
				return nil
			})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			if err := InstallSelected(ctx, &out, []string{"opencode"}, false); err != nil {
				t.Fatal(err)
			}
			if got := npmCleanupLines(out.String()); len(got) != 0 {
				t.Errorf("unverified local install planned cleanup: %q", got)
			}
			assertNpmNotRun(t, marker)
		})
	}
}
