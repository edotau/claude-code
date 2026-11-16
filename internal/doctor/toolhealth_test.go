package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/tools"
)

// Given a present and a missing host tool, When doctor renders them, Then it shows path+version or the install command.
func TestDepDoctorDetail(t *testing.T) {
	for _, tc := range []struct {
		s    tools.DepStatus
		want string
	}{
		{tools.DepStatus{Name: "shfmt", Path: "/b/shfmt", Version: "v3.14.1"}, "/b/shfmt v3.14.1"},
		{tools.DepStatus{Name: "tmux", Path: "/b/tmux"}, "/b/tmux"},
		{tools.DepStatus{Name: "ruff", Class: "feature", Use: "format-stop on .py files", Fix: "brew install ruff"},
			"missing (feature: format-stop on .py files); install: brew install ruff"},
	} {
		if got := depDoctorDetail(tc.s); got != tc.want {
			t.Errorf("depDoctorDetail(%s) = %q, want %q", tc.s.Name, got, tc.want)
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
