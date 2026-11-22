package doctor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
)

// Given host tools present or missing, When doctor renders them, Then a missing optional one is ⚠ and a required one ✗.
func TestDepDoctorDetail(t *testing.T) {
	for _, tc := range []struct {
		s        tools.DepStatus
		wantPass bool
		want     string
		mark     string
	}{
		{tools.DepStatus{Name: "shfmt", Path: "/b/shfmt", Version: "v3.14.1"}, true, "/b/shfmt v3.14.1", "✓"},
		{tools.DepStatus{Name: "tmux", Path: "/b/tmux"}, true, "/b/tmux", "✓"},
		{tools.DepStatus{Name: "ruff", Class: "feature", Use: "format-stop on .py files", Fix: "brew install ruff"},
			false, "warn missing (feature: format-stop on .py files); install: brew install ruff", "⚠"},
		{tools.DepStatus{Name: "git", Class: "required", Use: "worktrees", Fix: "xcode-select --install", Required: true},
			false, "missing (required: worktrees); install: xcode-select --install", "✗"},
	} {
		pass, got := depDoctorDetail(tc.s)
		if pass != tc.wantPass || got != tc.want {
			t.Errorf("depDoctorDetail(%s) = %v %q, want %v %q", tc.s.Name, pass, got, tc.wantPass, tc.want)
		}
		var b strings.Builder
		printCheckLines(&b, []check{{Name: "dep " + tc.s.Name, OK: pass, Detail: got}})
		if !strings.HasPrefix(b.String(), tc.mark) {
			t.Errorf("row for %s = %q, want mark %s", tc.s.Name, b.String(), tc.mark)
		}
	}
}

// Given the host-tool table, When doctor's tool checks run, Then every host tool gets one "dep <name>" row.
func TestCheckToolHealthListsDeps(t *testing.T) {
	rows := map[string]string{}
	checkToolHealth(context.Background(), func(_, _ bool, name, detail string) {
		if strings.HasPrefix(name, "dep ") {
			rows[name] = detail
		}
	})
	all := tools.DepStatusAll()
	if len(rows) != len(all) {
		t.Fatalf("doctor has %d dep rows, want %d: %v", len(rows), len(all), rows)
	}
	for _, s := range all {
		if _, ok := rows["dep "+s.Name]; !ok {
			t.Errorf("doctor has no row for %s", s.Name)
		}
	}
}

// Given codex's help lacks one declared flag, When doctor runs its tool checks, Then the row names it and the version.
func TestCheckToolHealthReportsFlagDrift(t *testing.T) {
	hermetic(t)
	help, err := os.ReadFile("testdata/codex-exec-help.txt")
	if err != nil {
		t.Fatal(err)
	}
	orig := agentHelp
	t.Cleanup(func() { agentHelp = orig })
	text := string(help)
	agentHelp = func(_ context.Context, spec agent.Schema) (string, string, error) {
		if spec.Name != "codex" {
			return "", "", tools.ErrAgentNotInstalled
		}
		return "0.166.0", text, nil
	}
	row := func() (bool, string) {
		var got *check
		checkToolHealth(context.Background(), func(ok, hard bool, name, d string) {
			if name == "agent flags codex" {
				got = &check{Name: name, OK: ok, Hard: hard, Detail: d}
			}
		})
		if got == nil {
			t.Fatal("no flag row for codex")
		}
		return got.OK, got.Detail
	}
	if ok, got := row(); !ok || got != "ok" {
		t.Errorf("full help = %v %q, want ok", ok, got)
	}
	text = strings.ReplaceAll(text, "--ephemeral", "--ephemeral-run")
	if ok, got := row(); ok || got != "drift: --ephemeral (installed 0.166.0)" {
		t.Errorf("help without --ephemeral = %v %q, want drift naming it (a longer flag does not count)", ok, got)
	}
	agentHelp = func(context.Context, agent.Schema) (string, string, error) {
		return "", "", errors.New("no help output")
	}
	if _, got := row(); got != "skipped: help unavailable" {
		t.Errorf("failed help = %q, want skipped", got)
	}
}
