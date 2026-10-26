package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/route"
)

// TestGatewayProxyFlagValidation: lifecycle flags are mutually exclusive and --port belongs to --serve.
func TestGatewayProxyFlagValidation(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--start", "--stop"}, "at most one of"},
		{[]string{"--stop", "--status"}, "at most one of"},
		{[]string{"--stop", "--start", "--status"}, "at most one of"},
		{[]string{"--port", "9999"}, "--port needs --serve"},
		{[]string{"--status", "--port", "9999"}, "--port needs --serve"},
		{[]string{"--start", "--port", "9999"}, "--port needs --serve"},
		{[]string{"--start", "--format", "json"}, "applies to --status only"},
		{[]string{"--stop", "--format", "json"}, "applies to --status only"},
	}
	for _, c := range cases {
		code, stdout, stderr := captureOutput(t, func() int { return cmdGateway(append([]string{"proxy"}, c.args...)) })
		if code == 0 || !strings.Contains(stderr, c.want) || stdout != "" {
			t.Errorf("proxy %v: want error %q, got exit %d stdout %q stderr %q", c.args, c.want, code, stdout, stderr)
		}
	}
}

// TestGatewayProxyNoFlagIsStatus: no flag reports status and exits 1 while the router is down.
func TestGatewayProxyNoFlagIsStatus(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, _ := captureOutput(t, func() int { return cmdGateway([]string{"proxy"}) })
	if code != 1 || !strings.Contains(stdout, "not running") {
		t.Fatalf("want exit 1 + 'not running', got %d %q", code, stdout)
	}
	code, stdout, _ = captureOutput(t, func() int { return cmdGateway([]string{"proxy", "--status", "--format", "json"}) })
	var ps map[string]any
	if err := json.Unmarshal([]byte(stdout), &ps); err != nil || code != 1 || ps["running"] != false {
		t.Fatalf("status json: exit %d err %v out %q", code, err, stdout)
	}
}

// TestGatewayBareShowsRouteAndProxyFooter: bare gateway exits 0 even with the router down, ending in the proxy lines.
func TestGatewayBareShowsRouteAndProxyFooter(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, stderr := captureOutput(t, func() int { return cmdGateway(nil) })
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	n := len(lines)
	if n < 4 || !strings.HasPrefix(lines[0], "tier ") ||
		!strings.HasPrefix(lines[n-2], "router    not running") || !strings.HasPrefix(lines[n-1], "fallback") {
		t.Fatalf("unexpected bare output:\n%s", stdout)
	}
}

// TestGatewayBareJSONShape: bare --format json nests the unchanged route payload beside the proxy state.
func TestGatewayBareJSONShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, _ := captureOutput(t, func() int { return cmdGateway([]string{"--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		Route map[string]json.RawMessage `json:"route"`
		Proxy map[string]json.RawMessage `json:"proxy"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	for _, k := range []string{"tier", "weights", "candidates", "sources"} {
		if _, ok := got.Route[k]; !ok {
			t.Errorf("route payload missing %q", k)
		}
	}
	for _, k := range []string{"running", "fallback"} {
		if _, ok := got.Proxy[k]; !ok {
			t.Errorf("proxy payload missing %q", k)
		}
	}
	// gateway route alone stays the bare route payload (no wrapper).
	_, rout, _ := captureOutput(t, func() int { return cmdGateway([]string{"route", "--format", "json"}) })
	var plain map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rout), &plain); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := plain["route"]; wrapped {
		t.Error("gateway route json must not be wrapped")
	}
	if _, ok := plain["tier"]; !ok {
		t.Error("gateway route json lost its top-level tier")
	}
}

// TestGatewayUnknownWord: a stray word is an error, not a silent route pick.
func TestGatewayUnknownWord(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdGateway([]string{"bogus"}) })
	if code == 0 || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

// TestPrintRouteWidthsComeFromData: a 40-char model id must not push the following columns out of line.
func TestPrintRouteWidthsComeFromData(t *testing.T) {
	long := strings.Repeat("m", 40)
	res := route.Result{Tier: "sonnet", Weights: map[string]float64{},
		Candidates: []route.Candidate{
			{Runner: "claude", Model: "sonnet", Account: "a", State: "ok", Scores: map[string]float64{}},
			{Runner: "opencode", Model: long, Account: "b", State: "ok", Scores: map[string]float64{}},
		}}
	_, out, _ := captureOutput(t, func() int { printRoute(res, routeSources{}); return 0 })
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "#") || strings.HasPrefix(l, "1 ") || strings.HasPrefix(l, "2 ") {
			rows = append(rows, l)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("want header + 2 rows, got %d:\n%s", len(rows), out)
	}
	col := strings.Index(rows[0], "EFFORT")
	for _, r := range rows[1:] {
		if i := strings.Index(r, " - "); i < 0 || i+1 != col {
			t.Errorf("EFFORT column misaligned (want %d) in %q", col, r)
		}
	}
	if !strings.Contains(rows[2], long+" ") {
		t.Errorf("long model truncated: %q", rows[2])
	}
}

// TestPrintRouteSkipHeader: skipped runners get a SKIP GATE REASON header and aligned rows without a "skip" prefix.
func TestPrintRouteSkipHeader(t *testing.T) {
	res := route.Result{Tier: "sonnet", Weights: map[string]float64{},
		Skipped: []route.Skip{{Runner: "copilot", Gate: "exhausted", Reason: "r1"}, {Runner: "x", Gate: "g", Reason: "r2"}}}
	_, out, _ := captureOutput(t, func() int { printRoute(res, routeSources{}); return 0 })
	if !strings.Contains(out, "SKIP    GATE      REASON\n") ||
		!strings.Contains(out, "copilot exhausted r1\n") || !strings.Contains(out, "x       g         r2\n") {
		t.Errorf("skip block wrong:\n%s", out)
	}
	_, none, _ := captureOutput(t, func() int { printRoute(route.Result{Weights: map[string]float64{}}, routeSources{}); return 0 })
	if strings.Contains(none, "SKIP") {
		t.Errorf("no skips must print no header:\n%s", none)
	}
}
