package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
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

// depWant is each row's dry-run line per platform (darwin: go beats brew); {local} is ~/.local/bin, {prefix} ~/.local.
var depWant = map[string][3]string{ // darwin, linux, windows
	"git":  {"skipped: report only (required); run: xcode-select --install", "skipped: report only (required); run: sudo apt-get install -y git", ""},
	"go":   {"ok /usr/local/go/bin/go", "ok /usr/bin/go", ""},
	"make": {"skipped: report only (required); run: xcode-select --install", "skipped: report only (required); run: sudo apt-get install -y make", ""},
	"curl": {"skipped: report only (required); run: xcode-select --install", "skipped: report only (required); run: sudo apt-get install -y curl", ""},
	"shfmt": {"would install: GOBIN={local} go install mvdan.cc/sh/v3/cmd/shfmt@v3.14.1",
		"would install: GOBIN={local} go install mvdan.cc/sh/v3/cmd/shfmt@v3.14.1", ""},
	"ruff": {"would install: brew install ruff", "would install: pipx install ruff==0.17.5", ""},
	"node": {"would install: brew install node", "skipped: none of brew on PATH; run: sudo apt-get install -y nodejs", ""},
	"prettier": {"would install: brew install prettier",
		"would install: npm install --global --prefix {prefix} --no-fund --no-audit prettier@3.10.1", ""},
	"gh": {"would install: GOBIN={local} go install github.com/cli/cli/v2/cmd/gh@v2.104.0",
		"would install: GOBIN={local} go install github.com/cli/cli/v2/cmd/gh@v2.104.0", ""},
	"tmux":          {"would install: brew install tmux", "skipped: none of brew on PATH; run: sudo apt-get install -y tmux", ""},
	"shellcheck":    {"would install: brew install shellcheck", "skipped: none of brew on PATH; run: sudo apt-get install -y shellcheck", ""},
	"golangci-lint": {"skipped: report only (gate); run: make install DEV_TOOLS=1", "skipped: report only (gate); run: make install DEV_TOOLS=1", ""},
	"govulncheck":   {"skipped: report only (gate); run: make install DEV_TOOLS=1", "skipped: report only (gate); run: make install DEV_TOOLS=1", ""},
	"yamlfmt":       {"skipped: report only (gate); run: make install DEV_TOOLS=1", "skipped: report only (gate); run: make install DEV_TOOLS=1", ""},
}

// Given each host tool missing, When install --tools --missing --dry-run runs per platform, Then each prints its route.
func TestInstallDepsDryRunPerPlatform(t *testing.T) {
	for i, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			home, ran := stubDeps(t, goos, depInstallers[goos], nil)
			local := filepath.Join(home, ".local", "bin")
			for _, d := range deps {
				want, ok := depWant[d.Name]
				if !ok {
					t.Fatalf("dep %q has no expectation in depWant", d.Name)
				}
				line := want[i]
				if goos == "windows" {
					line = "skipped: Windows reports only; run: " + depFix(d)
				}
				line = strings.NewReplacer("{local}", local, "{prefix}", filepath.Dir(local)).Replace(line)
				var out bytes.Buffer
				if err := Install(t.Context(), &out, []string{d.Name}, true, true); err != nil {
					t.Fatal(err)
				}
				if got := strings.TrimSpace(strings.TrimPrefix(out.String(), "== host tools ==\n")); got != d.Name+strings.Repeat(" ", 15-len(d.Name))+line {
					t.Errorf("%s: got %q, want line %q", d.Name, got, line)
				}
			}
			if len(*ran) != 0 {
				t.Errorf("dry-run ran %v; want nothing", *ran)
			}
		})
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
	want := "GOBIN=" + filepath.Join(home, ".local", "bin") + " /usr/bin/go install github.com/cli/cli/v2/cmd/gh@v2.104.0"
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
	if len(*ran) != 1 || (*ran)[0] != "/u/uv tool install ruff==0.17.5" {
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
