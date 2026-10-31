package agent

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

	"github.com/edotau/claude-code/internal/paths"
)

type toolsTestEnv struct {
	home   string
	config string
	path1  string
	path2  string
}

func setupToolsTestEnv(t *testing.T) toolsTestEnv {
	t.Helper()
	env := toolsTestEnv{
		home:   t.TempDir(),
		config: t.TempDir(),
		path1:  t.TempDir(),
		path2:  t.TempDir(),
	}
	t.Setenv("HOME", env.home)
	t.Setenv("CLAUDE_CONFIG_DIR", env.config)
	setToolsPath(t, env.path1, env.path2)
	return env
}

func setToolsPath(t *testing.T, dirs ...string) {
	t.Helper()
	dirs = append(dirs, "/usr/bin", "/bin")
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
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
	env := setupToolsTestEnv(t)
	bin := filepath.Join(env.home, ".local", "bin")
	marker := filepath.Join(root, "npm-called")
	writeToolsFixture(t, filepath.Join(bin, "npm"),
		"#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+strconv.Quote(marker)+"\n", 0o755)
	setToolsPath(t, bin, env.path1, env.path2)
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
			root := t.TempDir()
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
			setToolsPath(t,
				localBin, missingBin, filepath.Join(nonexecPrefix, "bin"), filepath.Join(dirPrefix, "bin"),
				filepath.Join(prefixA, "bin"), alias, filepath.Join(prefixA, "bin"), filepath.Join(prefixB, "bin"),
			)

			if tc.name == "codex" {
				calls := 0
				stubToolsInstall(t, tc.name, func(_ context.Context, _ io.Writer, _ string, dryRun bool) error {
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
			// The code prints symlink-resolved prefixes; macOS TempDir lives under /var → /private/var.
			realA, realB := mustEvalSymlinks(t, prefixA), mustEvalSymlinks(t, prefixB)
			want := []string{
				"  npm      uninstall -g --prefix " + realA + " " + tc.pkg,
				"  npm      uninstall -g --prefix " + realB + " " + tc.pkg,
			}
			if got := npmCleanupLines(out.String()); !reflect.DeepEqual(got, want) {
				t.Errorf("cleanup lines = %q, want %q\noutput:\n%s", got, want, out.String())
			}
			assertNpmNotRun(t, marker)
		})
	}
}

func TestInstallSelectedFailureSkipsNpmCleanup(t *testing.T) {
	root := t.TempDir()
	localBin, marker := isolateToolsEnv(t, root)
	writeToolsFixture(t, filepath.Join(localBin, "opencode"), "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	prefix := npmToolsFixture(t, root, "opencode", "opencode-ai")
	setToolsPath(t, localBin, filepath.Join(prefix, "bin"))
	wantErr := errors.New("installer failed")
	stubToolsInstall(t, "opencode", func(context.Context, io.Writer, string, bool) error { return wantErr })

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
			root := t.TempDir()
			localBin, marker := isolateToolsEnv(t, root)
			prefix := npmToolsFixture(t, root, "opencode", "opencode-ai")
			setToolsPath(t, localBin, filepath.Join(prefix, "bin"))
			localPath := filepath.Join(prefix, "bin", "opencode")
			stubToolsInstall(t, "opencode", func(_ context.Context, _ io.Writer, dir string, dryRun bool) error {
				if dryRun {
					t.Error("expected a real-run version guard")
				}
				if got := filepath.Join(dir, "opencode"); got != localPath {
					t.Errorf("local install path = %q, want %q", got, localPath)
				}
				if err := os.Remove(localPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if tc.script != "" {
					writeToolsFixture(t, localPath, tc.script, 0o755)
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
			wantKeep := "  keep     npm-global opencode (" + localPath + " did not answer --version)"
			if !strings.Contains(out.String(), wantKeep) {
				t.Errorf("keep line = %q, want to name %q", out.String(), localPath)
			}
			assertNpmNotRun(t, marker)
		})
	}
}

func TestInstallDir(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pathCopy   bool
		direct     bool
		wantVendor bool
	}{
		{name: "claude"},
		{name: "codex", pathCopy: true, direct: true, wantVendor: true},
		{name: "codex", pathCopy: true, wantVendor: true},
		{name: "copilot", wantVendor: true},
		{name: "claude-science"},
	} {
		t.Run(tc.name+"-"+strconv.FormatBool(tc.direct)+"-"+strconv.FormatBool(tc.pathCopy), func(t *testing.T) {
			env := setupToolsTestEnv(t)
			tool, ok := LookupTool(tc.name)
			if !ok {
				t.Fatalf("LookupTool(%q) failed", tc.name)
			}
			if tc.pathCopy {
				writeToolsFixture(t, filepath.Join(env.path1, tool.Binary), "#!/bin/sh\nexit 0\n", 0o755)
			}
			if tc.direct {
				writeToolsFixture(t, filepath.Join(paths.BinDir(), tool.Binary), "#!/bin/sh\nexit 0\n", 0o755)
			}

			want := filepath.Join(env.home, ".local", "bin")
			if tc.wantVendor {
				want = paths.BinDir()
			}
			if tc.name == "codex" && tc.pathCopy && !tc.direct {
				want = env.path1
			}
			var oldPath string
			if tc.pathCopy {
				oldPath = filepath.Join(env.path1, tool.Binary)
			}
			if got := installDir(tool, oldPath); got != want {
				t.Errorf("installDir(%s, %q) = %q, want %q", tc.name, oldPath, got, want)
			}
		})
	}
}

func TestInstallSelectedCopilotDryRunReportsInstallDir(t *testing.T) {
	for _, pathCopy := range []bool{false, true} {
		t.Run("path-copy-"+strconv.FormatBool(pathCopy), func(t *testing.T) {
			env := setupToolsTestEnv(t)
			if pathCopy {
				writeToolsFixture(t, filepath.Join(env.path1, "copilot"), "#!/bin/sh\nexit 0\n", 0o755)
			}
			wantDir := paths.BinDir()
			if pathCopy {
				wantDir = env.path1
			}
			stubToolsInstall(t, "copilot", func(_ context.Context, _ io.Writer, dir string, dryRun bool) error {
				if !dryRun {
					t.Error("expected dry-run installer")
				}
				if dir != wantDir {
					t.Errorf("install dir = %q, want %q", dir, wantDir)
				}
				return nil
			})

			var out bytes.Buffer
			if err := InstallSelected(context.Background(), &out, []string{"copilot"}, true); err != nil {
				t.Fatal(err)
			}
			if want := "  into     " + wantDir; !strings.Contains(out.String(), want) {
				t.Errorf("output = %q, want line containing %q", out.String(), want)
			}
		})
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
