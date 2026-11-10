package databricks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/providers"
)

const exHost = "https://example.cloud.databricks.com"

func noTokens(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "ANTHROPIC_AUTH_TOKEN") || strings.Contains(s, "eyJ") {
		t.Errorf("token-like string in output:\n%s", s)
	}
}

func TestConfigStdoutClaudeAndCodex(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, out, _ := runCaptured(t, func() int {
		return config([]string{"claude", "--host", exHost, "--wire", "serving", "--auth", "cli", "--set", "FOO=1", "--unset", "ENABLE_TOOL_SEARCH"})
	})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	noTokens(t, out)
	for _, w := range []string{exHost + "/serving-endpoints/anthropic", `"FOO": "1"`, "databricks auth token --host " + exHost} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in\n%s", w, out)
		}
	}
	if strings.Contains(out, "ENABLE_TOOL_SEARCH") {
		t.Error("unset ignored")
	}
	code, out, _ = runCaptured(t, func() int {
		return config([]string{"codex", "--host", exHost, "--auth", "cli"})
	})
	if code != 0 || !strings.Contains(out, "[model_providers.databricks.auth]") || !strings.Contains(out, exHost+"/serving-endpoints") {
		t.Errorf("codex %d:\n%s", code, out)
	}
	noTokens(t, out)
}

func TestConfigGatewayNoteOnStderr(t *testing.T) {
	_, out, errOut := runCaptured(t, func() int {
		return config([]string{"claude", "--host", exHost, "--wire", "ai-gateway", "--auth", "cli"})
	})
	if !strings.Contains(errOut, "budget-denied") || strings.Contains(out, "budget-denied") {
		t.Errorf("note placement: out=%q err=%q", out, errOut)
	}
	if !strings.Contains(out, exHost+"/ai-gateway/anthropic") {
		t.Errorf("gateway url missing:\n%s", out)
	}
}

func TestConfigRefusals(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := map[string][]string{
		"bad target":  {"vim", "--host", exHost},
		"cred --set":  {"claude", "--host", exHost, "--set", "ANTHROPIC_AUTH_TOKEN=x"},
		"bad wire":    {"claude", "--host", exHost, "--wire", "nope"},
		"bad auth":    {"claude", "--host", exHost, "--auth", "nope"},
		"non-claude":  {"claude", "--host", exHost, "--auth", "cli", "--model", "gpt-5.5"},
		"claude->cdx": {"codex", "--host", exHost, "--auth", "cli", "--model", "claude-opus-5-5"},
		"old --env":   {"claude", "--env", "dev", "--host", exHost},
		"old gateway": {"claude", "--wire", "gateway", "--host", exHost},
		"http host":   {"claude", "--host", "http://example.com", "--auth", "cli"},
	}
	for name, a := range cases {
		code, out, _ := runCaptured(t, func() int { return config(a) })
		if code != 2 {
			t.Errorf("%s: code %d", name, code)
		}
		if out != "" {
			t.Errorf("%s: wrote stdout %q", name, out)
		}
	}
}

func TestConfigOutGuard(t *testing.T) {
	out := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(out, []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"claude", "--host", exHost, "--auth", "cli", "--out", out}
	if code := config(args); code != 2 {
		t.Fatalf("overwrite w/o --force: %d", code)
	}
	if b, _ := os.ReadFile(out); string(b) != "KEEP" {
		t.Errorf("file clobbered: %q", b)
	}
	if code := config(append(args, "--force")); code != 0 {
		t.Fatalf("force: %d", code)
	}
	b, _ := os.ReadFile(out)
	noTokens(t, string(b))
	if !strings.Contains(string(b), exHost) {
		t.Errorf("not rewritten:\n%s", b)
	}
	fresh := filepath.Join(t.TempDir(), "n.json")
	if code := config([]string{"claude", "--host", exHost, "--auth", "cli", "--out", fresh}); code != 0 {
		t.Fatal(code)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func fakeClaudeOnPath(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "claude"), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", d)
}

func TestRescuePrintNeverExecs(t *testing.T) {
	fakeClaudeOnPath(t)
	f := &fakeChild{}
	useFake(t, f)
	code, out, _ := runCaptured(t, func() int { return rescue([]string{"--host", exHost, "--print", "--", "-p", "hi"}) })
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if len(f.calls) != 0 {
		t.Fatalf("--print spawned: %v", f.calls)
	}
	noTokens(t, out)
	i := strings.Index(out, "claude --settings ")
	if i < 0 {
		t.Fatalf("no command line:\n%s", out)
	}
	file := strings.Fields(out[i:])[2]
	if !strings.HasPrefix(file, os.TempDir()) {
		t.Errorf("scratch outside temp: %s", file)
	}
	t.Cleanup(func() { os.Remove(file) })
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("scratch %v %v", fi, err)
	}
	if !strings.Contains(out, "-p hi") || !strings.Contains(out, "databricks auth token --host "+exHost) {
		t.Errorf("args/auth default missing:\n%s", out)
	}
}

func TestRescueRunsThroughSeamWithScrubbedEnv(t *testing.T) {
	fakeClaudeOnPath(t)
	t.Setenv("ANTHROPIC_BASE_URL", "http://leak")
	t.Setenv("HARNESS_OPUS_MODEL", "leak")
	f := &fakeChild{code: 7}
	useFake(t, f)
	code, _, _ := runCaptured(t, func() int { return rescue([]string{"--host", exHost, "--", "-p", "hi"}) })
	if code != 7 || len(f.calls) != 1 {
		t.Fatalf("code %d calls %d", code, len(f.calls))
	}
	c := f.calls[0]
	t.Cleanup(func() { os.Remove(argValue(c.argv, "--settings")) })
	if !strings.HasSuffix(c.bin, "claude") || c.argv[0] != "--settings" || c.argv[len(c.argv)-1] != "hi" {
		t.Errorf("call %+v", c)
	}
	for _, kv := range c.env {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "HARNESS_") {
			t.Errorf("env leaked %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

func TestRescueRefusals(t *testing.T) {
	fakeClaudeOnPath(t)
	f := &fakeChild{}
	useFake(t, f)
	for name, a := range map[string][]string{
		"cred set":   {"--host", exHost, "--set", "MY_SECRET=1"},
		"non-claude": {"--host", exHost, "--model", "gpt-5.5"},
		"bad wire":   {"--host", exHost, "--wire", "x"},
	} {
		if code, _, _ := runCaptured(t, func() int { return rescue(a) }); code != 2 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(f.calls) != 0 {
		t.Error("refused rescue spawned")
	}
	t.Setenv("PATH", t.TempDir())
	if code, _, _ := runCaptured(t, func() int { return rescue([]string{"--host", exHost}) }); code != 2 || len(f.calls) != 0 {
		t.Errorf("missing claude: %d", code)
	}
}

// rescueScratch runs rescue with a recording spawn; it returns the scratch path and whether it existed mid-run.
func rescueScratch(t *testing.T, spawnErr error) (path string, existedDuring bool, code int) {
	t.Helper()
	fakeClaudeOnPath(t)
	old := spawn
	spawn = func(_ string, argv, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
		path = argValue(argv, "--settings")
		_, err := os.Stat(path)
		existedDuring = err == nil
		return 3, spawnErr
	}
	t.Cleanup(func() { spawn = old })
	code, _, _ = runCaptured(t, func() int { return rescue([]string{"--host", exHost}) })
	t.Cleanup(func() { os.Remove(path) })
	return path, existedDuring, code
}

func TestRescueDeletesScratchAfterChildExits(t *testing.T) {
	path, during, code := rescueScratch(t, nil)
	if code != 3 || path == "" || !during {
		t.Fatalf("code %d path %q existedDuring %v", code, path, during)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scratch survived the child: %v", err)
	}
}

func TestRescueDeletesScratchWhenSpawnFails(t *testing.T) {
	path, _, code := rescueScratch(t, errors.New("boom"))
	if code != 2 || path == "" {
		t.Fatalf("code %d path %q", code, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scratch survived a failed spawn: %v", err)
	}
}

func TestApiKeyHelperQuotesPathWithSpace(t *testing.T) {
	o := base(t, TargetClaude, "serving")
	o.Self = "/opt/my tools/claude-code"
	r, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		K string `json:"apiKeyHelper"`
	}
	if err := json.Unmarshal([]byte(r.Text), &d); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.K, "'/opt/my tools/claude-code' resolve --profile ") {
		t.Errorf("helper path not quoted: %q", d.K)
	}
	o.Self = "/bin/cc"
	if r, _ = Render(o); strings.Contains(r.Text, "'/bin/cc'") {
		t.Errorf("a safe path must stay unquoted:\n%s", r.Text)
	}
}

func TestSetRejectsKeysThatBreakOutOfTOML(t *testing.T) {
	for _, k := range []string{"a b", `a"b`, "a]b", "a\nb", "[table]", "", "é"} {
		for _, target := range []string{TargetCodex, TargetClaude} {
			o := base(t, target, "serving")
			o.Set = []string{k + "=v"}
			if _, err := Render(o); err == nil {
				t.Errorf("%s: --set key %q accepted", target, k)
			}
		}
	}
	o := base(t, TargetCodex, "serving")
	o.Set = []string{"model_reasoning_effort=high"}
	if _, err := Render(o); err != nil {
		t.Errorf("plain key refused: %v", err)
	}
}

func TestRescueScrubsInheritedAuthEnv(t *testing.T) {
	environ := []string{
		"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_OAUTH_TOKEN=x", "CLAUDE_CONFIG_DIR=/c",
		"DATABRICKS_TOKEN=x", "DATABRICKS_HOST=h", "DATABRICKS_CONFIG_PROFILE=p", "HARNESS_PROVIDER=p", "CLAUDE_PROFILE=dev", "CLAUDE_OPUS_MODEL=m", "ANTHROPIC_API_KEY=x",
		"PATH=/bin", "HOME=/h", "CLAUDE_CODE_NO_FLICKER=1", "MY_DATABRICKS_NOTE=keep",
	}
	got := scrubbed(environ)
	want := []string{"PATH=/bin", "HOME=/h", "CLAUDE_CODE_NO_FLICKER=1", "MY_DATABRICKS_NOTE=keep"}
	if !slices.Equal(got, want) {
		t.Errorf("scrubbed = %v, want %v", got, want)
	}
}

// A probe timeout is named as such, not the bare "network error".
func TestProbeFailureNamesTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := probeFailure(ctx, errors.New("x")); got != "network error: timeout" {
		t.Errorf("probeFailure = %q", got)
	}
	if got := probeFailure(context.Background(), &net.DNSError{Name: "h"}); got != "network error: DNS" {
		t.Errorf("DNS probeFailure = %q", got)
	}
}

func TestCheckAllAcceptsPositionalModel(t *testing.T) {
	seedOverlay(t, "")
	t.Setenv("DATABRICKS_TEST_PROBE_TOKEN", "test-token")
	seen := make(chan string, 2)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seen <- body.Model
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	old := probeClient
	probeClient = srv.Client()
	t.Cleanup(func() { probeClient = old })
	if err := providers.SetProviderFields("test-probe", map[string]any{
		"preset": "databricks", "host": srv.URL,
		"auth": map[string]any{"type": "key", "env": "DATABRICKS_TEST_PROBE_TOKEN"},
	}); err != nil {
		t.Fatal(err)
	}
	model := "claude-opus-5-5"
	code, out, stderr := runCaptured(t, func() int {
		return Run([]string{"check", model, "--all", "--wire", "all", "--profile", "test-probe", "--model", "claude-haiku-4-5"})
	})
	if code != 0 || len(seen) != 2 {
		t.Fatalf("exit = %d, probes = %d, stdout = %q, stderr = %q", code, len(seen), out, stderr)
	}
	got := []string{<-seen, <-seen} // the wires probe concurrently
	slices.Sort(got)
	if want := []string{"databricks-" + model, "system.ai." + model}; !slices.Equal(got, want) {
		t.Errorf("probe models = %q, want positional model %q", got, want)
	}
}

func TestProbeReportsCutOffBody(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(status)
				if _, err := w.Write([]byte("partial")); err != nil {
					t.Errorf("write partial response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			p := &providers.Provider{Name: "p", Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL}
			if got, want := probe(context.Background(), p, "test-token", "m"), fmt.Sprintf("HTTP %d (body cut off)", status); got != want {
				t.Errorf("probe = %q, want %q", got, want)
			}
		})
	}
}

// probeServer is a TLS probe endpoint that holds each reply for delay and counts requests.
func probeServer(t *testing.T, delay time.Duration) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	old := probeClient
	probeClient = srv.Client()
	t.Cleanup(func() { probeClient = old })
	t.Setenv("DATABRICKS_TEST_PROBE_TOKEN", "test-token")
	for _, name := range []string{"p1", "p2", "p3"} {
		if err := providers.SetProviderFields(name, map[string]any{"preset": "databricks", "host": srv.URL,
			"auth": map[string]any{"type": "key", "env": "DATABRICKS_TEST_PROBE_TOKEN"}}); err != nil {
			t.Fatal(err)
		}
	}
	return &n
}

// Profiles and their wires probe concurrently, and rows keep the requested order.
func TestCheckProfilesConcurrentInOrder(t *testing.T) {
	seedOverlay(t, "")
	const delay = 300 * time.Millisecond
	n := probeServer(t, delay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"p3", "p1", "p2"}
	start := time.Now()
	rows := checkProfiles(reg, names, "claude-haiku-5-5", []string{providers.WireServing, providers.WireAIGateway})
	if took := time.Since(start); took >= 2*delay || n.Load() != 6 {
		t.Fatalf("3 profiles x 2 wires took %s with %d probes; serial would be %s", took, n.Load(), 6*delay)
	}
	for i, r := range rows {
		if len(r.cells) != 2 || r.cells[0] != "ok" || r.cells[1] != "ok" {
			t.Errorf("row %d (%s) = %+v", i, names[i], r)
		}
	}
}

// Without --wire only the serving wire is probed and the header names only it; a bad --wire is refused.
func TestCheckAllDefaultsToServing(t *testing.T) {
	seedOverlay(t, "")
	n := probeServer(t, 0)
	code, out, _ := runCaptured(t, func() int { return Run([]string{"check", "--all", "--profile", "p1"}) })
	if code != 0 || n.Load() != 1 || !strings.Contains(out, "SERVING") || strings.Contains(out, "AI-GATEWAY") {
		t.Fatalf("exit = %d, probes = %d, stdout = %q", code, n.Load(), out)
	}
	if code, _, _ := runCaptured(t, func() int { return Run([]string{"check", "--all", "--wire", "bogus"}) }); code == 0 {
		t.Fatal("--wire bogus accepted")
	}
}

// A check's token request reports a 429 after one attempt; it never waits out the dispatch retries.
func TestCheckProfileTokenNoRetry(t *testing.T) {
	seedOverlay(t, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	oldBackoff := oauth.RetryBackoff
	oauth.RetryBackoff = time.Millisecond
	t.Cleanup(func() { oauth.RetryBackoff = oldBackoff })
	t.Setenv("DATABRICKS_NORETRY_M2M", "client-secret")
	row := &providers.Provider{Name: "noretry", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic,
		BaseURL: srv.URL + "/serving-endpoints/anthropic", Auth: providers.Auth{Type: providers.AuthM2M, ClientID: "c"}}
	r := checkProfile(row, "noretry", "m", []string{providers.WireServing})
	if r.cells[0] != "no token" || calls.Load() != 1 {
		t.Fatalf("cells = %q, token calls = %d, want no token after 1 call", r.cells, calls.Load())
	}
	if _, err := oauth.MintM2M(context.Background(), srv.URL, "c", "client-secret"); err == nil || calls.Load() != 4 {
		t.Fatalf("a dispatch mint must keep its retries: calls = %d", calls.Load())
	}
}
