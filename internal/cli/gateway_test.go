package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	if n < 4 || !strings.HasPrefix(lines[0], "model ") ||
		!strings.HasPrefix(lines[n-2], "proxy     not running") || !strings.HasPrefix(lines[n-1], "fallback") {
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
	for _, k := range []string{"model", "weights", "candidates", "sources"} {
		if _, ok := got.Route[k]; !ok {
			t.Errorf("route payload missing %q", k)
		}
	}
	for _, k := range []string{"running", "fallback"} {
		if _, ok := got.Proxy[k]; !ok {
			t.Errorf("proxy payload missing %q", k)
		}
	}
	// gateway router alone stays the bare route payload (no wrapper).
	_, rout, _ := captureOutput(t, func() int { return cmdGateway([]string{"router", "--format", "json"}) })
	var plain map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rout), &plain); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := plain["route"]; wrapped {
		t.Error("gateway router json must not be wrapped")
	}
	if _, ok := plain["model"]; !ok {
		t.Error("gateway router json lost its top-level model")
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
	res := router.PickResult{Tier: "sonnet", Weights: map[string]float64{},
		Candidates: []router.Candidate{
			{Agent: "claude", Model: "sonnet", Account: "a", State: "ok", Scores: map[string]float64{}},
			{Agent: "opencode", Model: long, Account: "b", State: "ok", Scores: map[string]float64{}},
		}}
	_, out, _ := captureOutput(t, func() int { printRoute(res, routeSources{}); return 0 })
	var rows []string
	for l := range strings.SplitSeq(out, "\n") {
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
	res := router.PickResult{Tier: "sonnet", Weights: map[string]float64{},
		Skipped: []router.Skip{{Agent: "copilot", Gate: "exhausted", Reason: "r1"}, {Agent: "x", Gate: "g", Reason: "r2"}}}
	_, out, _ := captureOutput(t, func() int { printRoute(res, routeSources{}); return 0 })
	if !strings.Contains(out, "SKIP    GATE      REASON\n") ||
		!strings.Contains(out, "copilot exhausted r1\n") || !strings.Contains(out, "x       g         r2\n") {
		t.Errorf("skip block wrong:\n%s", out)
	}
	_, none, _ := captureOutput(t, func() int { printRoute(router.PickResult{Weights: map[string]float64{}}, routeSources{}); return 0 })
	if strings.Contains(none, "SKIP") {
		t.Errorf("no skips must print no header:\n%s", none)
	}
}

func stubToken(t *testing.T, mint func(context.Context, string, int) (string, error)) {
	t.Helper()
	oe, os_, om := tokenEnsure, tokenState, tokenMint
	t.Cleanup(func() { tokenEnsure, tokenState, tokenMint = oe, os_, om })
	tokenEnsure = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	tokenState = func(context.Context) (proxy.State, bool) { return proxy.State{PID: 4242}, true }
	tokenMint = mint
}

func TestGatewayTokenPrintsOnlyDaemonBoundSessionToken(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "CLAUDE_CODE_ROUTER_SECRET", "long-lived-router-secret")
	var gotProfile string
	var gotPID int
	stubToken(t, func(_ context.Context, p string, pid int) (string, error) {
		gotProfile, gotPID = p, pid
		return "cct_abc", nil
	})
	code, stdout, stderr := captureOutput(t, func() int { return cmdGateway([]string{"token", "--profile", "dev"}) })
	if code != 0 || stdout != "cct_abc\n" || stderr != "" || gotProfile != "dev" || gotPID != 4242 {
		t.Fatalf("exit %d stdout %q stderr %q profile %q pid %d", code, stdout, stderr, gotProfile, gotPID)
	}
}

func TestGatewayTokenRejectsBadArgsAndMintFailure(t *testing.T) {
	vaulttest.Use(t)
	stubToken(t, func(context.Context, string, int) (string, error) { return "", errors.New("boom") })
	for _, args := range [][]string{{"token"}, {"token", "extra"}, {"token", "--profile", "dev"}} {
		code, stdout, _ := captureOutput(t, func() int { return cmdGateway(args) })
		if code == 0 || stdout != "" {
			t.Fatalf("%v: exit %d stdout %q", args, code, stdout)
		}
	}
}

func TestDesktopAndTokenHelpFitsEightyColumns(t *testing.T) {
	for _, args := range [][]string{{"configure", "desktop", "--help"}, {"gateway", "token", "--help"}} {
		code, stdout, stderr := captureOutput(t, func() int { return Run(append([]string{"claude-code"}, args...)) })
		if code != 0 || stdout != "" {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
		for line := range strings.SplitSeq(stderr, "\n") {
			if termWidth(line) > 80 {
				t.Errorf("%v: line exceeds 80 columns: %s", args, line)
			}
		}
	}
}
