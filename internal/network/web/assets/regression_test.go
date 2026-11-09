package assets

import (
	_ "embed"
	"os/exec"
	"strings"
	"testing"
)

//go:embed index.html.tmpl
var dashboardTemplate string

func runDashboardRegression(t *testing.T, scenario string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("dashboard regression tests require node on PATH")
	}
	for _, args := range [][]string{
		{"--check", "testdata/dashboard_regressions.cjs"},
		{"testdata/dashboard_regressions.cjs", scenario},
	} {
		cmd := exec.Command(node, args...)
		cmd.Stdin = strings.NewReader(dashboardTemplate)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node %v: %v\n%s", args, err, out)
		}
	}
}

func TestOpenDetailIgnoresStaleSnapshot(t *testing.T) {
	runDashboardRegression(t, "detail")
}

func TestShowPlanIgnoresStaleFailure(t *testing.T) {
	runDashboardRegression(t, "planFailure")
}

func TestPostTickDoesNotReopenPreviousPlan(t *testing.T) {
	runDashboardRegression(t, "tick")
}

func TestLoadUsageModulesRetriesAfterFailure(t *testing.T) {
	runDashboardRegression(t, "usage")
}
