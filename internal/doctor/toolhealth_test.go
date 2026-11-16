package doctor

import (
	"context"
	"strings"
	"testing"

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
