package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stubDeps fakes PATH with onPath (name -> path) and the platform, and records every installer command.
func stubDeps(t *testing.T, goos string, onPath map[string]string, runErr error) (home string, ran *[]string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oldGOOS, oldLook, oldRun := goosOverride, depLookPath, depRun
	t.Cleanup(func() { goosOverride, depLookPath, depRun = oldGOOS, oldLook, oldRun })
	goosOverride = goos
	depLookPath = func(name string) (string, error) {
		if p, ok := onPath[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
	ran = &[]string{}
	depRun = func(_ context.Context, w io.Writer, env []string, name string, args ...string) error {
		*ran = append(*ran, strings.Join(append(append(append([]string(nil), env...), name), args...), " "))
		_, _ = io.WriteString(w, "progress\nlast words\n")
		return runErr
	}
	return home, ran
}

// depInstallers are the installers each platform scenario has on PATH.
var depInstallers = map[string]map[string]string{
	"darwin":  {"brew": "/opt/homebrew/bin/brew", "go": "/usr/local/go/bin/go"},
	"linux":   {"go": "/usr/bin/go", "npm": "/usr/bin/npm", "pipx": "/usr/bin/pipx", "apt-get": "/usr/bin/apt-get"},
	"windows": {},
}

// depNamed is the deps row called name.
func depNamed(t *testing.T, name string) dep {
	t.Helper()
	for _, d := range deps {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no dep row %q", name)
	return dep{}
}

// pinOnPath is the table pin of d's first pinned installer found in on, or "".
func pinOnPath(d dep, on map[string]string) string {
	for _, p := range []struct {
		pin  string
		bins []string
	}{{d.Go, []string{"go"}}, {d.Npm, []string{"npm"}}, {d.Py, []string{"uv", "pipx"}}} {
		for _, b := range p.bins {
			if p.pin != "" && on[b] != "" {
				return p.pin
			}
		}
	}
	return ""
}

// checkDryLine asserts d's dry-run line from the deps row: present, pinned route, brew fallback, or skipped.
func checkDryLine(t *testing.T, d dep, goos string, on map[string]string, line string) {
	t.Helper()
	pin := pinOnPath(d, on)
	switch {
	case goos == "windows":
		if !strings.HasPrefix(line, "skipped: Windows reports only") {
			t.Errorf("%s: %q; want a Windows report-only skip", d.Name, line)
		}
	case on[d.Name] != "":
		if line != "ok "+on[d.Name] {
			t.Errorf("%s: %q; want ok at its PATH entry", d.Name, line)
		}
	case pin != "":
		if !strings.HasPrefix(line, "would install: ") || !strings.Contains(line, pin) || strings.Contains(line, "brew") {
			t.Errorf("%s: %q; want the pinned route %q, not brew", d.Name, line, pin)
		}
	case d.Brew != "" && on["brew"] != "":
		if line != "would install: brew install "+d.Brew {
			t.Errorf("%s: %q; want the brew fallback for %q", d.Name, line, d.Brew)
		}
	case !strings.HasPrefix(line, "skipped: "):
		t.Errorf("%s: %q; want a skip with no runnable route", d.Name, line)
	}
}

// Given each host tool missing, When install --tools --missing --dry-run runs per platform, Then each prints its route.
func TestInstallDepsDryRunPerPlatform(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			_, ran := stubDeps(t, goos, depInstallers[goos], nil)
			for _, d := range deps {
				var out bytes.Buffer
				if err := Install(t.Context(), &out, []string{d.Name}, true, true); err != nil {
					t.Fatal(err)
				}
				got := strings.TrimSpace(strings.TrimPrefix(out.String(), "== host tools ==\n"))
				checkDryLine(t, d, goos, depInstallers[goos], strings.TrimSpace(strings.TrimPrefix(got, d.Name)))
			}
			if len(*ran) != 0 {
				t.Errorf("dry-run ran %v; want nothing", *ran)
			}
		})
	}
}

// Given every dep, When routes are listed, Then pinned routes carry the table pin and come before brew, the fallback.
func TestDepRoutesPinnedBeforeBrew(t *testing.T) {
	stubDeps(t, "linux", nil, nil)
	for _, d := range deps {
		rs := depRoutes(d)
		brewAt := -1
		for i, r := range rs {
			if r.bin == "brew" {
				brewAt = i
			}
		}
		if d.Brew != "" && brewAt != len(rs)-1 {
			t.Errorf("%s: brew at %d of %d routes; want it last", d.Name, brewAt, len(rs))
		}
		for _, pin := range []string{d.Go, d.Npm, d.Py} {
			if pin != "" && !slices.ContainsFunc(rs, func(r route) bool { return slices.Contains(r.args, pin) }) {
				t.Errorf("%s: no route carries pin %q", d.Name, pin)
			}
		}
	}
}

// Given a tool already on PATH, When install --tools runs (missing or not), Then it says ok and runs nothing.
func TestInstallDepPresentIsOK(t *testing.T) {
	_, ran := stubDeps(t, "darwin", map[string]string{"brew": "/b/brew", "shfmt": "/b/shfmt"}, nil)
	for _, missing := range []bool{true, false} {
		var out bytes.Buffer
		if err := Install(t.Context(), &out, []string{"shfmt"}, false, missing); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "shfmt          ok /b/shfmt") || len(*ran) != 0 {
			t.Errorf("missing=%v: out %q ran %v; want ok and no command", missing, out.String(), *ran)
		}
	}
}

// Given a real run, When the installer succeeds or fails, Then argv is exact and a failure never fails the install.
func TestInstallDepRunsRouteAsArgv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runErr error
		want   string
	}{
		{"success", nil, "shfmt          installed: brew install shfmt (not on PATH yet; add its bin dir to PATH)"},
		{"failure", errors.New("exit status 1"), "shfmt          skipped: exit status 1 (last words); run: brew install shfmt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ran := stubDeps(t, "darwin", map[string]string{"brew": "/opt/homebrew/bin/brew"}, tc.runErr)
			var out bytes.Buffer
			if err := Install(t.Context(), &out, []string{"shfmt"}, false, true); err != nil {
				t.Fatalf("Install = %v; a host-tool failure must not fail the install", err)
			}
			if len(*ran) != 1 || (*ran)[0] != "/opt/homebrew/bin/brew install shfmt" {
				t.Errorf("ran %v; want the resolved brew with argv install shfmt", *ran)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("out %q; want %q", out.String(), tc.want)
			}
		})
	}
}

// Given go on PATH and no brew, When gh installs, Then go install gets GOBIN=~/.local/bin and the pinned module.
func TestInstallDepGoRouteSetsGOBIN(t *testing.T) {
	home, ran := stubDeps(t, "linux", map[string]string{"go": "/usr/bin/go"}, nil)
	if err := Install(t.Context(), io.Discard, []string{"gh"}, false, true); err != nil {
		t.Fatal(err)
	}
	want := "GOBIN=" + filepath.Join(home, ".local", "bin") + " /usr/bin/go install " + depNamed(t, "gh").Go
	if len(*ran) != 1 || (*ran)[0] != want {
		t.Errorf("ran %v; want %q", *ran, want)
	}
}

// Given uv and pipx both on PATH, When ruff installs, Then uv wins.
func TestInstallDepPrefersUVOverPipx(t *testing.T) {
	_, ran := stubDeps(t, "linux", map[string]string{"uv": "/u/uv", "pipx": "/u/pipx"}, nil)
	if err := Install(t.Context(), io.Discard, []string{"ruff"}, false, true); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 1 || (*ran)[0] != "/u/uv tool install "+depNamed(t, "ruff").Py {
		t.Errorf("ran %v; want uv tool install", *ran)
	}
}

// Given bare --tools --missing, When it runs, Then the host tools print under one header after the agent rows.
func TestInstallAllPrintsHostToolsOnce(t *testing.T) {
	stubDeps(t, "windows", nil, nil)
	for i := range tools {
		if tools[i].dep == nil {
			stubToolsInstall(t, tools[i].Name, func(context.Context, io.Writer, string, bool) error { return nil })
		}
	}
	var out bytes.Buffer
	_ = Install(t.Context(), &out, nil, true, true)
	if n := strings.Count(out.String(), "== host tools =="); n != 1 {
		t.Errorf("header printed %d times in %q; want 1", n, out.String())
	}
	if !strings.Contains(out.String(), "yamlfmt        skipped: Windows reports only") {
		t.Errorf("out %q; want the last host tool's line", out.String())
	}
}

// Given one present and one missing tool, When doctor reads DepStatusAll, Then it gets path or the fix, and required.
func TestDepStatusAll(t *testing.T) {
	stubDeps(t, "darwin", map[string]string{"brew": "/b/brew", "git": "/nonexistent/git"}, nil)
	got := map[string]DepStatus{}
	for _, s := range DepStatusAll() {
		got[s.Name] = s
	}
	if len(got) != len(deps) {
		t.Fatalf("DepStatusAll has %d rows; want %d", len(got), len(deps))
	}
	if g := got["git"]; g.Path != "/nonexistent/git" || g.Fix != "" || !g.Required {
		t.Errorf("git = %+v; want present, required, no fix", g)
	}
	if s := got["shfmt"]; s.Path != "" || s.Fix != "brew install shfmt" || s.Required || s.Class != "feature" {
		t.Errorf("shfmt = %+v; want missing feature with brew fix", s)
	}
}

// Given every route, When rendered, Then npm and go land under ~/.local and no route names sudo.
func TestDepRoutesNeverSudo(t *testing.T) {
	stubDeps(t, "linux", nil, nil)
	for _, d := range deps {
		for _, r := range depRoutes(d) {
			if r.bin == "sudo" || strings.Contains(r.String(), "sudo") {
				t.Errorf("%s route %q runs sudo", d.Name, r)
			}
		}
	}
}

// Given real --version output whose first line is no version, When picked, Then the tool's own version shows.
func TestDepPickUsesTheToolsOwnVersion(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"shellcheck", "ShellCheck - shell script analysis tool\nversion: 0.11.0\nlicense: GNU General Public License, version 3\nwebsite: https://www.shellcheck.net", "0.11.0"},
		{"govulncheck", "Go: go1.27.4\nScanner: govulncheck@v1.8.0\nDB: https://vuln.go.dev\nDB updated: 2026-11-11 02:51:11 +0000 UTC", "1.8.0"},
		{"go", "go version go1.27.4 darwin/arm64", "1.27.4"},
		{"tmux", "tmux 3.5a", "tmux 3.5a"},
	} {
		if got := depPick(depNamed(t, tc.name), tc.out); got != tc.want {
			t.Errorf("depPick(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
