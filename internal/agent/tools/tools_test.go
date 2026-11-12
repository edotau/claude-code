package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

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
	old := tools
	tools = append([]tool(nil), tools...)
	t.Cleanup(func() { tools = old })
	for i := range tools {
		if tools[i].Name == name {
			tools[i].Install = install
			tools[i].Update = nil
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

func TestNpmCleanupForCanceledCtxKeepsNpm(t *testing.T) {
	root := t.TempDir()
	_, marker := isolateToolsEnv(t, root)
	local := filepath.Join(t.TempDir(), "opencode")
	writeToolsFixture(t, local, "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer

	npmCleanupFor(ctx, &output, "opencode", filepath.Dir(local), false)
	if !strings.Contains(output.String(), "did not answer --version") {
		t.Errorf("canceled version probe output = %q; want keep diagnostic", output.String())
	}
	assertNpmNotRun(t, marker)
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
				stubSingleDryRunInstall(t, tc.name)
			}

			assertDryRunNpmPrefixes(t, tc.name, tc.pkg, prefixA, prefixB, marker)
		})
	}
}

func stubSingleDryRunInstall(t *testing.T, name string) {
	t.Helper()
	calls := 0
	stubToolsInstall(t, name, func(_ context.Context, _ io.Writer, _ string, dryRun bool) error {
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

func assertDryRunNpmPrefixes(t *testing.T, name, pkg, prefixA, prefixB, marker string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := Install(ctx, &out, []string{name}, true, false); err != nil {
		t.Fatal(err)
	}
	// The code prints symlink-resolved prefixes; macOS TempDir lives under /var → /private/var.
	realA, realB := mustEvalSymlinks(t, prefixA), mustEvalSymlinks(t, prefixB)
	want := []string{
		"  npm      uninstall -g --prefix " + realA + " " + pkg,
		"  npm      uninstall -g --prefix " + realB + " " + pkg,
	}
	if got := npmCleanupLines(out.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("cleanup lines = %q, want %q\noutput:\n%s", got, want, out.String())
	}
	assertNpmNotRun(t, marker)
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
	if err := Install(ctx, &out, []string{"opencode"}, false, false); !errors.Is(err, wantErr) {
		t.Fatalf("Install error = %v, want %v", err, wantErr)
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
			if err := Install(ctx, &out, []string{"opencode"}, false, false); err != nil {
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
			tool, ok := lookup(tc.name)
			if !ok {
				t.Fatalf("Lookup(%q) failed", tc.name)
			}
			if tc.pathCopy {
				writeToolsFixture(t, filepath.Join(env.path1, tool.Binary), "#!/bin/sh\nexit 0\n", 0o755)
			}
			if tc.direct {
				writeToolsFixture(t, filepath.Join(paths.BinDir(), tool.Binary), "#!/bin/sh\nexit 0\n", 0o755)
			}

			want := expectedInstallDir(env.home, env.path1, tc.name, tc.wantVendor, tc.pathCopy, tc.direct)
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

func expectedInstallDir(home, path1, name string, wantVendor, pathCopy, direct bool) string {
	want := filepath.Join(home, ".local", "bin")
	if wantVendor {
		want = paths.BinDir()
	}
	if name == "codex" && pathCopy && !direct {
		want = path1
	}
	return want
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
			if err := Install(context.Background(), &out, []string{"copilot"}, true, false); err != nil {
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

func TestLocalBinDirUnsetHomeIsNotCwdRelative(t *testing.T) {
	t.Setenv("HOME", "")
	if got := localBinDir(); got != "" {
		t.Fatalf("localBinDir() = %q, want empty when HOME is unset", got)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := installBinaryLocked(io.Discard, localBinDir(), "x", []byte("x")); err == nil {
		t.Fatal("installBinaryLocked with empty dir succeeded, want error")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".local")); !os.IsNotExist(err) {
		t.Fatalf("cwd-relative .local was created: %v", err)
	}
}

func TestStatusOneTargetIsLinkTargetOnly(t *testing.T) {
	env := setupToolsTestEnv(t)
	candidate := filepath.Join(env.path1, "codex")
	writeToolsFixture(t, candidate, "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	tool, ok := lookup("codex")
	if !ok {
		t.Fatal("codex tool is missing")
	}

	Link(&bytes.Buffer{}, []string{"codex"}, false)
	dst := filepath.Join(paths.BinDir(), "codex")
	wantTarget, err := os.Readlink(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOne(tool).Target; got != wantTarget {
		t.Errorf("linked Target = %q, want Readlink %q", got, wantTarget)
	}

	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	writeToolsFixture(t, dst, "#!/bin/sh\nprintf '1.0.0\\n'\n", 0o755)
	if got := statusOne(tool).Target; got != "" {
		t.Errorf("regular-file Target = %q, want empty", got)
	}
}

// Every roster agent's executable has an install row.
func TestToolsCoverEveryAgentBinary(t *testing.T) {
	for _, a := range agentid.All() {
		bin := agentid.Binary(a)
		if !slices.ContainsFunc(tools, func(tl tool) bool { return tl.Binary == bin }) {
			t.Errorf("no Tools row for %s's binary %q", a, bin)
		}
	}
}
