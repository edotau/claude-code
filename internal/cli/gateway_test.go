package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
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
		code, stdout, stderr := hosttest.CaptureOutput(t,
			func() int { return cmdGateway(append([]string{"proxy"}, c.args...)) })
		if code == 0 || !strings.Contains(stderr, c.want) || stdout != "" {
			t.Errorf("proxy %v: want error %q, got exit %d stdout %q stderr %q", c.args, c.want, code, stdout, stderr)
		}
	}
}

// TestGatewayProxyNoFlagIsStatus: no flag reports status and exits 1 while the router is down.
func TestGatewayProxyNoFlagIsStatus(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return cmdGateway([]string{"proxy"}) })
	if code != 1 || !strings.Contains(stdout, "not running") {
		t.Fatalf("want exit 1 + 'not running', got %d %q", code, stdout)
	}
	code, stdout, _ = hosttest.CaptureOutput(t,
		func() int { return cmdGateway([]string{"proxy", "--status", "--format", "json"}) })
	var ps map[string]any
	if err := json.Unmarshal([]byte(stdout), &ps); err != nil || code != 1 || ps["running"] != false {
		t.Fatalf("status json: exit %d err %v out %q", code, err, stdout)
	}
}

// TestGatewayBareShowsRouteAndProxyFooter: bare gateway exits 0 even with the router down, ending in the proxy lines.
func TestGatewayBareShowsRouteAndProxyFooter(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdGateway(nil) })
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
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return cmdGateway([]string{"--format", "json"}) })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdGateway([]string{"bogus"}) })
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
		Skipped: []router.Skip{
			{Agent: "copilot", Gate: "exhausted", Reason: "r1"},
			{Agent: "x", Gate: "g", Reason: "r2"},
		}}
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
	if err := printProxyState(gateway.Status{Log: "x.log"}); err == nil {
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
	tokenDaemon(t, func(http.ResponseWriter, gateway.SessionRequest) {})
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
	for _, f := range gatewaySpec().Sub["proxy"].Flags {
		have[f.Name] = true
	}
	for _, name := range []string{"start", "stop", "restart", "status", "serve", "port"} {
		if fs.Lookup(name) == nil || !have[name] {
			t.Errorf("flag %q: defined=%v completed=%v", name, fs.Lookup(name) != nil, have[name])
		}
	}
}

// Stale copilot accounts refresh through ask.RouteRefresh, so these tests stay offline.
func init() {
	copilot.RefreshAccount = func(ctx context.Context, key string) bool { return ask.RouteRefresh(ctx, key) }
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureRouteStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), code
}

// routeJSONFixture installs a work copilot profile, fake copilot+codex binaries and a 402-exhausted account.
func routeJSONFixture(t *testing.T) {
	t.Helper()
	dir := vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"work":{"protocol":"openai",
		"base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},"login":"work-user"}}}}`)
	vaulttest.SetRecord(t, "copilot", "profile", "work")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"copilot", "codex"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", b), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ask.RouteRefresh = func(context.Context, string) bool { return false }
	ghHosts(t, "work-user")
	t.Cleanup(func() { ask.RouteRefresh = nil })
	if err := router.MarkExhausted("github·work", time.Now().Add(240*time.Hour), time.Now(), "402"); err != nil {
		t.Fatal(err)
	}
}

func assertRouteJSONSkipsAndLast(t *testing.T, res routeJSON) {
	t.Helper()
	var skip *router.Skip
	for i := range res.Skipped {
		if res.Skipped[i].Agent == "copilot" {
			skip = &res.Skipped[i]
		}
	}
	if skip == nil || skip.Gate != router.GateExhausted ||
		!strings.Contains(skip.Reason, "github·work exhausted until") {
		t.Errorf("copilot skip = %+v in %+v", skip, res.Skipped)
	}
	if c := res.Candidates[len(res.Candidates)-1]; c.Agent != "claude" || c.Model != "claude-sonnet-5-5" ||
		c.Scores == nil {
		t.Errorf("last candidate = %+v, want claude at sonnet with scores", c)
	}
}

// TestRouteVerbJSON: the pick's JSON carries weights, per-dimension scores, totals and each skip's gate.
func TestRouteVerbJSON(t *testing.T) {
	routeJSONFixture(t)
	out, code := captureRouteStdout(t, func() int {
		return gatewayRoute([]string{"--model", "sonnet", "--agent", "copilot", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res routeJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.Model != "sonnet" || res.Weights["price"] == 0 || len(res.Candidates) == 0 {
		t.Fatalf("res = %+v", res)
	}
	assertRouteJSONSkipsAndLast(t, res)
	if code := gatewayRoute([]string{"--model", "fable"}); code == 0 {
		t.Error("an unrouted alias must fail")
	}
}

type routeSourcesJSON struct {
	Sources struct {
		Weights string `json:"weights"`
	} `json:"sources"`
}

func fetchRouteSources(t *testing.T) routeSourcesJSON {
	t.Helper()
	out, code := captureRouteStdout(t,
		func() int { return gatewayRoute([]string{"--model", "sonnet", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var got routeSourcesJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return got
}

func assertDefaultSources(t *testing.T, got routeSourcesJSON) {
	t.Helper()
	if got.Sources.Weights != "default" {
		t.Errorf("sources (no overlay) = %+v, want weights default", got.Sources)
	}
}

// TestRouteVerbSourcesNameWhereEachKnobCameFrom: the weights source reads default until the vault overlay sets them.
func TestRouteVerbSourcesNameWhereEachKnobCameFrom(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	assertDefaultSources(t, fetchRouteSources(t))

	if err := providers.UpdateRouting(func(r *providers.Routing) error {
		r.Weights = map[string]float64{"price": 0.5, "headroom": 0.5} // equal to the default: provenance is still "vault"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	withOverlay := fetchRouteSources(t)
	if withOverlay.Sources.Weights != "vault" {
		t.Errorf("weights source = %q, want vault even though the value equals the default",
			withOverlay.Sources.Weights)
	}

	textOut, code := captureRouteStdout(t, func() int { return gatewayRoute([]string{"--model", "sonnet"}) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, textOut)
	}
	if !strings.Contains(textOut, "(vault)") {
		t.Errorf("text output = %q, want it to name the vault weights source", textOut)
	}
}

// ghHosts makes user the only gh login: a scratch hosts.yml under GH_CONFIG_DIR.
func ghHosts(t *testing.T, user string) {
	t.Helper()
	dir := t.TempDir()
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)
}

// TestRouteEndsNamesResolvedClaudeAccount: the router view resolves claude's actual backend, reported as .ends.
func TestRouteEndsNamesResolvedClaudeAccount(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(router.EnvRoute, router.RouteDefault) // claude on a Databricks account means a Databricks session's ladder
	oldAcct, oldRefresh := agent.ResolveAccount, ask.RouteRefresh
	agent.ResolveAccount = func(ag string, _ router.Cell) (string, string) {
		if ag == "claude" {
			return "databricks·prod", ""
		}
		return "", ""
	}
	ask.RouteRefresh = func(context.Context, string) bool { return false }
	t.Cleanup(func() { agent.ResolveAccount, ask.RouteRefresh = oldAcct, oldRefresh })

	out, code := captureRouteStdout(t,
		func() int { return gatewayRoute([]string{"--model", "sonnet", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res routeJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.Ends != "databricks·prod" {
		t.Errorf("ends = %q, want databricks·prod\n%s", res.Ends, out)
	}
}

// TestRouterStartHelpDoesNotStart: `gateway proxy -h` printed nothing and spawned the daemon (seen 2026-10-23 when
// an opencode audit dispatch asked for help). The flag must print usage and leave the router alone.
func TestRouterStartHelpDoesNotStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"proxy", "-h"}, {"proxy", "--help"}, {"-h"}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdGateway(args) })
		if code != 0 || !strings.Contains(stderr, "usage: claude-code gateway") {
			t.Errorf("gateway %v: want usage + exit 0, got exit %d stderr %q", args, code, stderr)
		}
	}
	if _, ok := gateway.Running(context.Background()); ok {
		t.Fatal("router was started by a help request")
	}
}

// TestRouterStrayArgumentIsAUsageError: a typo after start/stop/status fails loudly instead of being ignored.
func TestRouterStrayArgumentIsAUsageError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdGateway([]string{"proxy", "now"}) })
	if code == 0 || !strings.Contains(stderr, `unexpected argument "now"`) {
		t.Fatalf("want a usage error naming the stray argument, got exit %d stderr %q", code, stderr)
	}
}

func TestGatewayRouteRejectsStrayArgument(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bare", []string{"--model", "sonnet", "typo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdGateway(tc.args) })
			if code != 1 || stdout != "" || !strings.Contains(stderr, `unexpected argument "typo"`) {
				t.Fatalf("want exit 1, no route output and stray-argument error; got exit %d stdout %q stderr %q",
					code, stdout, stderr)
			}
		})
	}
}

// parityEnv signs in a Databricks profile (gpt-6-astra Responses-capable or not); agent stubs mark any prompt run.
func parityEnv(t *testing.T, profile string, responses bool) (marker string) {
	t.Helper()
	dir := vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", profile)
	marker = filepath.Join(t.TempDir(), "ran")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"codex", "copilot", "opencode", "gemini"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", b), []byte("#!/bin/sh\ncase \"$1\" in exec|run|-p|--prompt) touch "+marker+";; esac\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+profile+`":{"preset":"databricks",`+
		`"host":"https://profile.example.com","auth":{"type":"pat"},"models":{"sonnet":"gpt-6-1-sol"}}}}}`)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{
		{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol", Responses: true},
		{ID: "gpt-6-astra", Label: "GPT-6 Astra", Responses: responses},
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers[profile] == nil {
		t.Fatalf("profile absent: %v", err)
	}
	vaulttest.Secret(t, providers.AuthSecretName(reg.Providers[profile]), "profile-auth")
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "chatgpt"}); err != nil { // own login = on
		t.Fatal(err)
	}
	old := ask.RouteRefresh
	ask.RouteRefresh = func(context.Context, string) bool { return false }
	t.Cleanup(func() { ask.RouteRefresh = old })
	return marker
}

// assertPreviewMatchesChain compares the previewed candidates and skips against the dispatch chain; returns the skips.
func assertPreviewMatchesChain(t *testing.T, got routeJSON, chain agent.Chain, out string) string {
	t.Helper()
	if len(got.Candidates) != len(chain.Dispatches) {
		t.Fatalf("preview has %d candidates, dispatch %d\n%s", len(got.Candidates), len(chain.Dispatches), out)
	}
	for i, disp := range chain.Dispatches {
		c := got.Candidates[i]
		if c.Agent != disp.Agent.Name() || c.Account != disp.Account || (c.Model != disp.Model && c.Agent != "claude") {
			t.Errorf("#%d preview %s/%s/%s, dispatch %s/%s/%s", i+1, c.Agent, c.Account, c.Model,
				disp.Agent.Name(), disp.Account, disp.Model)
		}
	}
	var skips []string
	for _, s := range got.Skipped {
		skips = append(skips, s.Agent+": "+s.Reason)
	}
	if strings.Join(skips, "\n") != strings.Join(chain.Skipped, "\n") {
		t.Errorf("preview skips %q, dispatch skips %q", skips, chain.Skipped)
	}
	return strings.Join(skips, "\n")
}

// TestRouterPreviewMatchesDispatch: `gateway` lists the candidates, models and skips `ask` would route.
func TestRouterPreviewMatchesDispatch(t *testing.T) {
	cases := []struct {
		name, model string
		responses   bool
		wantSkip    string // a skip naming this profile must appear
	}{
		{"exact_supported", "gpt-6-astra", true, ""},
		{"exact_unsupported", "gpt-6-astra", false, "astra-par"},
		{"alias_control", "sonnet", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := parityEnv(t, "astra-par", tc.responses)
			out, code := captureRouteStdout(t, func() int {
				return gatewayRoute(
					[]string{"--agent", "codex", "--model", tc.model, "--fallback", "agents", "--format", "json"})
			})
			if code != 0 {
				t.Fatalf("preview exit %d: %s", code, out)
			}
			var got routeJSON
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			chain, _, err := agent.RouteChainFor(context.Background(), "codex", tc.model, "sonnet",
				router.FallbackAgents, false,
				router.PickOpts{Refresh: ask.RouteRefresh, WorkRepo: func() bool { return router.IsWorkRepo("") }})
			if err != nil {
				t.Fatal(err)
			}
			skips := assertPreviewMatchesChain(t, got, chain, out)
			if tc.wantSkip != "" && !strings.Contains(skips, tc.wantSkip) {
				t.Errorf("no skip names profile %s: %q", tc.wantSkip, skips)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("preview ran an agent on a prompt")
			}
		})
	}
}

// stubRefresh swaps ask.RouteRefresh for fn (restoring the prior value) and returns a call counter.
func stubRefresh(t *testing.T, fn func(key string) bool) *int {
	t.Helper()
	prev := ask.RouteRefresh
	calls := 0
	ask.RouteRefresh = func(_ context.Context, key string) bool { calls++; return fn(key) }
	t.Cleanup(func() { ask.RouteRefresh = prev })
	return &calls
}

const usedPctKey = "github·alice"

// recordUsage stores a copilot usage entry for the account recorded age ago, in a fresh config dir.
func recordUsage(t *testing.T, pct float64, age time.Duration) {
	t.Helper()
	now := time.Now()
	if err := router.Record(usedPctKey, pct, "month", now.Add(24*time.Hour), now.Add(-age)); err != nil {
		t.Fatal(err)
	}
}

func usedPctConfig(t *testing.T) { t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) }

func TestCopilotUsedPctFreshEntryNoRefresh(t *testing.T) {
	usedPctConfig(t)
	recordUsage(t, 42.5, time.Minute)
	calls := stubRefresh(t, func(string) bool { return true })
	if pct, ok := copilot.CopilotUsedPct(context.Background(), "alice"); !ok || pct != 42.5 || *calls != 0 {
		t.Errorf("got (%v, %v), refreshes %d; want (42.5, true), 0", pct, ok, *calls)
	}
}

func TestCopilotUsedPctActiveExhaustedCountsAs100(t *testing.T) {
	usedPctConfig(t)
	recordUsage(t, 10, time.Minute)
	if err := router.MarkExhausted(usedPctKey, time.Now().Add(time.Hour), time.Now(), "402"); err != nil {
		t.Fatal(err)
	}
	calls := stubRefresh(t, func(string) bool { return true })
	if pct, ok := copilot.CopilotUsedPct(context.Background(), "alice"); !ok || pct != router.ExhaustedPct ||
		*calls != 0 {
		t.Errorf("got (%v, %v), refreshes %d; want (100, true), 0", pct, ok, *calls)
	}
}

func TestCopilotUsedPctExpiredExhaustedIgnored(t *testing.T) {
	usedPctConfig(t)
	now := time.Now()
	if err := router.MarkExhausted(usedPctKey, now.Add(-time.Hour), now.Add(-2*time.Hour), "402"); err != nil {
		t.Fatal(err)
	}
	recordUsage(t, 7, time.Minute)
	stubRefresh(t, func(string) bool { return true })
	if pct, ok := copilot.CopilotUsedPct(context.Background(), "alice"); !ok || pct != 7 {
		t.Errorf("got (%v, %v), want (7, true)", pct, ok)
	}
}

func TestCopilotUsedPctStaleRefreshesThenReloads(t *testing.T) {
	usedPctConfig(t)
	recordUsage(t, 10, router.StaleAfter+time.Minute)
	calls := stubRefresh(t, func(k string) bool {
		if k != usedPctKey {
			t.Errorf("refresh key = %q, want %q", k, usedPctKey)
		}
		return router.Record(usedPctKey, 61, "month", time.Now().Add(24*time.Hour), time.Now()) == nil
	})
	if pct, ok := copilot.CopilotUsedPct(context.Background(), "alice"); !ok || pct != 61 || *calls != 1 {
		t.Errorf("got (%v, %v), refreshes %d; want (61, true), 1", pct, ok, *calls)
	}
}

func TestCopilotUsedPctStaleRefreshFailsUnverified(t *testing.T) {
	usedPctConfig(t)
	recordUsage(t, 10, router.StaleAfter+time.Minute)
	stubRefresh(t, func(string) bool { return false })
	if pct, ok := copilot.CopilotUsedPct(context.Background(), "alice"); ok || pct != 0 {
		t.Errorf("got (%v, %v), want (0, false)", pct, ok)
	}
}

func TestCopilotUsedPctNoEntryRefreshFailsUnverified(t *testing.T) {
	usedPctConfig(t)
	calls := stubRefresh(t, func(string) bool { return false })
	if _, ok := copilot.CopilotUsedPct(context.Background(), "alice"); ok || *calls != 1 {
		t.Errorf("ok=%v refreshes %d; want false, 1", ok, *calls)
	}
}

func TestCopilotUsedPctNoEntryRefreshWithoutRecordUnverified(t *testing.T) {
	usedPctConfig(t)
	stubRefresh(t, func(string) bool { return true })
	if _, ok := copilot.CopilotUsedPct(context.Background(), "alice"); ok {
		t.Error("a never-seen account must stay unverified")
	}
}

// TestRenderGatewayRouteFailsWhenStdoutFull: a failed stdout write gives a nonzero exit, not 0.
func TestRenderGatewayRouteFailsWhenStdoutFull(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	orig := os.Stdout
	os.Stdout = full
	t.Cleanup(func() { os.Stdout = orig })
	res := router.PickResult{Alias: "m", Weights: map[string]float64{}}
	if code := renderGatewayRoute("gateway", res, ""); code == 0 {
		t.Fatal("exit 0 despite a failed stdout write")
	}
}

// TestNewRouteJSONEndsIsFirstClaudeAccount: JSON ends agrees with the text view's first claude candidate.
func TestNewRouteJSONEndsIsFirstClaudeAccount(t *testing.T) {
	cl := func(acct string) router.Candidate {
		return router.Candidate{Agent: "claude", Account: acct}
	}
	res := router.PickResult{Candidates: []router.Candidate{cl("free-acct"), cl("paid-acct")}}
	got := newRouteJSON(res, routeSources{}, nil)
	if got.Ends != "free-acct" || len(got.Candidates) != 2 {
		t.Fatalf("ends %q candidates %d, want free-acct and 2", got.Ends, len(got.Candidates))
	}
}

// TestGatewayRouteUnknownProfileIsAUsageError: a --profile that cannot lead exits 2 and names it, with no route output.
func TestGatewayRouteUnknownProfileIsAUsageError(t *testing.T) {
	vaulttest.Use(t)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return gatewayRoute([]string{"--profile", "nope"}) })
	if code != 2 || stdout != "" || !strings.Contains(stderr, "nope") {
		t.Fatalf("want exit 2, no stdout, stderr naming the profile; got exit %d stdout %q stderr %q", code, stdout,
			stderr)
	}
}

// tokenDaemon answers only health probes and token requests, without starting a daemon.
func tokenDaemon(t *testing.T, mint func(http.ResponseWriter, gateway.SessionRequest)) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Secret(t, "CLAUDE_CODE_ROUTER_SECRET", "long-lived-router-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set(gateway.PIDHeader, strconv.Itoa(os.Getpid()))
			return
		}
		if r.URL.Path != gateway.SessionPath || r.Header.Get("X-Api-Key") != "long-lived-router-secret" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var req gateway.SessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mint(w, req)
	}))
	t.Cleanup(srv.Close)
	cleanup, err := gateway.WriteState(gateway.State{PID: os.Getpid(), Port: srv.Listener.Addr().(*net.TCPAddr).Port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
}

func TestGatewayTokenPrintsOnlyDaemonBoundSessionToken(t *testing.T) {
	var gotProfile string
	var gotPID int
	tokenDaemon(t, func(w http.ResponseWriter, req gateway.SessionRequest) {
		gotProfile, gotPID = req.Profile, req.PID
		json.NewEncoder(w).Encode(gateway.SessionResponse{Token: "cct_abc"})
	})
	code, stdout, stderr := hosttest.CaptureOutput(t,
		func() int { return cmdGateway([]string{"token", "--profile", "dev"}) })
	if code != 0 || stdout != "cct_abc\n" || stderr != "" || gotProfile != "dev" || gotPID != os.Getpid() {
		t.Fatalf("exit %d stdout %q stderr %q profile %q pid %d", code, stdout, stderr, gotProfile, gotPID)
	}
}

func TestGatewayTokenRejectsBadArgsAndMintFailure(t *testing.T) {
	tokenDaemon(t,
		func(w http.ResponseWriter, _ gateway.SessionRequest) { w.WriteHeader(http.StatusInternalServerError) })
	for _, args := range [][]string{{"token"}, {"token", "extra"}, {"token", "--profile", "dev"}} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdGateway(args) })
		if code == 0 || stdout != "" || !strings.Contains(stderr, "gateway token:") {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

// TestPrintTokenWriteFailureNamesCause: a failed stdout write exits 1 with a stderr message.
func TestPrintTokenWriteFailureNamesCause(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	r, w, _ := os.Pipe()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = full, w
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })
	code := printToken("cct_abc")
	w.Close()
	msg, _ := io.ReadAll(r)
	if code != 1 || !strings.Contains(string(msg), "gateway token: write stdout") {
		t.Fatalf("want exit 1 with a 'gateway token: write stdout' message, got %d %q", code, msg)
	}
}

// TestProxyLifecycleRejectsFailedStdout: a failed stdout write exits nonzero, and restart still attempts the start.
func TestProxyLifecycleRejectsFailedStdout(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context) int
	}{{"stop", proxyStopHold}, {"restart", proxyRestart}} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Secret(t, "CLAUDE_CODE_ROUTER_SECRET", "restart-test-secret")
			if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			orig := gatewayStop
			gatewayStop = func(context.Context) (gateway.State, error) { return gateway.State{PID: 4242}, nil }
			t.Cleanup(func() { gatewayStop = orig })
			closed, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil || closed.Close() != nil {
				t.Fatal(err)
			}
			r, w, _ := os.Pipe()
			origOut, origErr := os.Stdout, os.Stderr
			os.Stdout, os.Stderr = closed, w
			code := tc.run(context.Background())
			os.Stdout, os.Stderr = origOut, origErr
			w.Close()
			msg, _ := io.ReadAll(r)
			if code == 0 || !strings.Contains(string(msg), "gateway proxy: write output:") {
				t.Fatalf("exit %d stderr %q; want nonzero with a write-output error", code, msg)
			}
			if tc.name == "restart" && !strings.Contains(string(msg), gateway.ErrTestBinary.Error()) {
				t.Fatalf("restart did not attempt the start after the failed write: %q", msg)
			}
		})
	}
}
