package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/router"
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
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(append([]string{"proxy"}, c.args...)) })
		if code == 0 || !strings.Contains(stderr, c.want) || stdout != "" {
			t.Errorf("proxy %v: want error %q, got exit %d stdout %q stderr %q", c.args, c.want, code, stdout, stderr)
		}
	}
}

// TestGatewayProxyNoFlagIsStatus: no flag reports status and exits 1 while the router is down.
func TestGatewayProxyNoFlagIsStatus(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"proxy"}) })
	if code != 1 || !strings.Contains(stdout, "not running") {
		t.Fatalf("want exit 1 + 'not running', got %d %q", code, stdout)
	}
	code, stdout, _ = hosttest.CaptureOutput(t, func() int { return Run([]string{"proxy", "--status", "--format", "json"}) })
	var ps map[string]any
	if err := json.Unmarshal([]byte(stdout), &ps); err != nil || code != 1 || ps["running"] != false {
		t.Fatalf("status json: exit %d err %v out %q", code, err, stdout)
	}
}

// TestGatewayBareShowsRouteAndProxyFooter: bare gateway exits 0 even with the router down, ending in the proxy lines.
func TestGatewayBareShowsRouteAndProxyFooter(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(nil) })
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

// TestGatewayBareJSONShape: --format json keeps the route fields at the top level beside the proxy state.
func TestGatewayBareJSONShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &top); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	for _, k := range []string{"model", "weights", "candidates", "sources", "proxy"} {
		if _, ok := top[k]; !ok {
			t.Errorf("payload missing top-level %q", k)
		}
	}
	var proxy map[string]json.RawMessage
	if err := json.Unmarshal(top["proxy"], &proxy); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"running", "fallback"} {
		if _, ok := proxy[k]; !ok {
			t.Errorf("proxy payload missing %q", k)
		}
	}
}

// TestGatewayUnknownWord: a stray word is an error, not a silent route pick.
func TestGatewayUnknownWord(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"bogus"}) })
	if code == 0 || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

// TestPrintRouteWidthsComeFromData: a 40-char model id must not push the following columns out of line.
func TestPrintRouteWidthsComeFromData(t *testing.T) {
	long := strings.Repeat("m", 40)
	res := router.PickResult{Alias: "sonnet", Weights: map[string]float64{},
		Candidates: []router.Candidate{
			{Agent: "claude", Model: "sonnet", Account: "a", State: "ok", Scores: map[string]float64{}},
			{Agent: "opencode", Model: long, Account: "b", State: "ok", Scores: map[string]float64{}},
		}}
	_, out, _ := hosttest.CaptureOutput(t, func() int { printRoute(res, routeSources{}, nil); return 0 })
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
	res := router.PickResult{Alias: "sonnet", Weights: map[string]float64{},
		Skipped: []router.Skip{{Agent: "copilot", Gate: "exhausted", Reason: "r1"}, {Agent: "x", Gate: "g", Reason: "r2"}}}
	_, out, _ := hosttest.CaptureOutput(t, func() int { printRoute(res, routeSources{}, nil); return 0 })
	if !strings.Contains(out, "SKIP    GATE      REASON\n") ||
		!strings.Contains(out, "copilot exhausted r1\n") || !strings.Contains(out, "x       g         r2\n") {
		t.Errorf("skip block wrong:\n%s", out)
	}
	_, none, _ := hosttest.CaptureOutput(t, func() int {
		printRoute(router.PickResult{Weights: map[string]float64{}}, routeSources{}, nil)
		return 0
	})
	if strings.Contains(none, "SKIP") {
		t.Errorf("no skips must print no header:\n%s", none)
	}
}

// TestPrintProxyStateReportsWriteError: a failed stdout write is returned, so bare gateway cannot exit 0.
func TestPrintProxyStateReportsWriteError(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	orig := os.Stdout
	os.Stdout = full
	t.Cleanup(func() { os.Stdout = orig })
	if err := printProxyState(proxy.Status{Log: "x.log"}); err == nil {
		t.Fatal("printProxyState returned nil despite a failed stdout write")
	}
}

// TestProxyStatusJSONReportsWriteError: a failed JSON write prints a message instead of a silent exit 1.
func TestProxyStatusJSONReportsWriteError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	r, w, _ := os.Pipe()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = full, w
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })
	code := proxyStatus(context.Background(), "json")
	w.Close()
	msg, _ := io.ReadAll(r)
	if code != 1 || !strings.Contains(string(msg), "gateway proxy:") {
		t.Fatalf("want exit 1 with a 'gateway proxy:' message, got %d %q", code, msg)
	}
}

// TestGatewayHelpListsProxyRestart: the bare help row for proxy names every lifecycle mode.
func TestGatewayHelpListsProxyRestart(t *testing.T) {
	var b strings.Builder
	gatewayPage.Fprint(&b)
	if !strings.Contains(b.String(), "--restart") {
		t.Errorf("gateway help omits --restart:\n%s", b.String())
	}
}

// TestProxyStartReportsWriteError: a failed stdout write of the base URL is an exit 1, not a silent 0.
func TestProxyStartReportsWriteError(t *testing.T) {
	tokenDaemon(t, func(http.ResponseWriter, proxy.SessionRequest) {})
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	r, w, _ := os.Pipe()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = full, w
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })
	code := proxyStart(context.Background())
	w.Close()
	msg, _ := io.ReadAll(r)
	if code != 1 || !strings.Contains(string(msg), "gateway proxy:") {
		t.Fatalf("want exit 1 with a 'gateway proxy:' message, got %d %q", code, msg)
	}
}

// TestSpecProxyCompletesRestart: the completion node lists every real proxy lifecycle flag.
func TestSpecProxyCompletesRestart(t *testing.T) {
	fs, _ := proxyFlags()
	have := map[string]bool{}
	for _, f := range Spec().Sub["proxy"].Flags {
		have[f.Name] = true
	}
	for _, name := range []string{"start", "stop", "restart", "status", "serve", "port"} {
		if fs.Lookup(name) == nil || !have[name] {
			t.Errorf("flag %q: defined=%v completed=%v", name, fs.Lookup(name) != nil, have[name])
		}
	}
}
