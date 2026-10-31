package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

func ep(id string) CopilotEndpoint {
	return CopilotEndpoint{ID: id, Name: id, Task: "llm/v1/chat", Ready: "READY", Tested: true}
}

func dbx() *providers.Provider {
	h := "https://h.example.com"
	return &providers.Provider{Preset: providers.PresetDatabricks, Headers: map[string]string{"X-H": "v"}, Routes: map[string]string{
		providers.ProtocolAnthropic: h, providers.ProtocolOpenAI: h + "/serving-endpoints", providers.ProtocolResponses: h + "/serving-endpoints"}}
}

type rendered struct {
	Providers []map[string]any `json:"providers"`
	Models    []map[string]any `json:"models"`
}

func render(t *testing.T, r CopilotRegistry, cred string) rendered {
	t.Helper()
	b, err := renderCopilotRegistry(r, cred)
	if err != nil {
		t.Fatal(err)
	}
	var out rendered
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuildRenderAnthropicOnly(t *testing.T) {
	r := copilotBuildRegistry("dev", dbx(), []CopilotEndpoint{ep("databricks-claude-sonnet-5-5")}, "http://127.0.0.1:9/")
	out := render(t, r, "cct_x")
	if len(out.Providers) != 1 {
		t.Fatalf("providers = %v, want one", out.Providers)
	}
	p := out.Providers[0]
	if p["name"] != "dev-anthropic" || p["type"] != "anthropic" || p["baseUrl"] != "http://127.0.0.1:9/p/dev" ||
		p["apiKey"] != "cct_x" || p["bearerToken"] != nil || p["wireApi"] != nil {
		t.Fatalf("anthropic provider = %v", p)
	}
	m := out.Models[0]
	if m["provider"] != "dev-anthropic" || m["wireModel"] != "databricks-claude-sonnet-5-5" || m["id"] != "claude-sonnet-5-5" ||
		m["maxOutputTokens"] != float64(copilotMaxOutputTokens) || m["maxContextWindowTokens"].(float64) <= 0 {
		t.Fatalf("model = %v", m)
	}
}

func TestBuildRenderMixedWires(t *testing.T) {
	eps := []CopilotEndpoint{ep("databricks-claude-sonnet-5-5"), ep("databricks-gpt-5-5"), ep("databricks-meta-llama-3-3-70b-instruct")}
	out := render(t, copilotBuildRegistry("dev", dbx(), eps, "http://r"), "cct_x")
	byName := map[string]map[string]any{}
	for _, p := range out.Providers {
		byName[p["name"].(string)] = p
	}
	if len(byName) != 3 {
		t.Fatalf("providers = %v, want anthropic, openai, responses", out.Providers)
	}
	a, o, rs := byName["dev-anthropic"], byName["dev-openai"], byName["dev-responses"]
	if a["apiKey"] != "cct_x" || a["bearerToken"] != nil {
		t.Errorf("anthropic cred placement: %v", a)
	}
	if o["type"] != "openai" || o["wireApi"] != "completions" || o["baseUrl"] != "http://r/p/dev/openai" ||
		o["bearerToken"] != "cct_x" || o["apiKey"] != nil {
		t.Errorf("openai provider: %v", o)
	}
	if rs["type"] != "openai" || rs["wireApi"] != "responses" || rs["baseUrl"] != "http://r/p/dev/responses" ||
		rs["bearerToken"] != "cct_x" || rs["apiKey"] != nil {
		t.Errorf("responses provider: %v", rs)
	}
	if len(out.Models) != 3 {
		t.Fatalf("models = %v", out.Models)
	}
}

func TestRenderEmptyRegistryErrors(t *testing.T) {
	if _, err := renderCopilotRegistry(CopilotRegistry{}, "x"); err == nil {
		t.Fatal("empty registry rendered")
	}
}

func TestBuildDropsRepeatedCanonicalIDs(t *testing.T) {
	r := copilotBuildRegistry("dev", dbx(), []CopilotEndpoint{ep("databricks-claude-sonnet-5-5"), ep("claude-sonnet-5-5")}, "http://r")
	if len(r.Models) != 1 {
		t.Fatalf("models = %v, want the duplicate dropped", r.Models)
	}
}

func fixture() CopilotRegistry {
	return CopilotRegistry{Profile: "dev", Models: []CopilotRegistryModel{
		{ID: "claude-sonnet-5-5", Provider: "dev-anthropic", WireModel: "databricks-claude-sonnet-5-5"},
		{ID: "gpt-5-5", Provider: "dev-responses", WireModel: "databricks-gpt-5-5"},
		{ID: "twin", Provider: "dev-openai", WireModel: "w-twin"},
		{ID: "twin", Provider: "dev-responses", WireModel: "w-twin2"},
	}}
}

func TestSelect(t *testing.T) {
	r := fixture()
	for _, tc := range []struct {
		name string
		c    CopilotChoice
		want string
		err  error
	}{
		{"alias", CopilotChoice{Requested: []string{"sonnet"}}, "dev-anthropic/claude-sonnet-5-5", nil},
		{"ref", CopilotChoice{Requested: []string{"dev-responses/gpt-5-5"}}, "dev-responses/gpt-5-5", nil},
		{"canonical", CopilotChoice{Requested: []string{"gpt-5-5"}}, "dev-responses/gpt-5-5", nil},
		{"wire", CopilotChoice{Requested: []string{"databricks-gpt-5-5"}}, "dev-responses/gpt-5-5", nil},
		{"pin", CopilotChoice{Pin: "gpt-5-5"}, "dev-responses/gpt-5-5", nil},
		{"saved", CopilotChoice{Saved: "dev-responses/gpt-5-5", Pin: "sonnet"}, "dev-responses/gpt-5-5", nil},
		{"requested beats saved", CopilotChoice{Requested: []string{"gpt-5-5"}, Saved: "sonnet"}, "dev-responses/gpt-5-5", nil},
		{"default", CopilotChoice{}, "dev-anthropic/claude-sonnet-5-5", nil},
		{"unknown", CopilotChoice{Requested: []string{"nope"}}, "", ErrUnknownModel},
		{"ambiguous", CopilotChoice{Requested: []string{"twin"}}, "", ErrAmbiguousModel},
		{"repeated", CopilotChoice{Requested: []string{"gpt-5-5", "sonnet"}}, "", ErrRepeatedSelection},
		{"saved gone", CopilotChoice{Saved: "dev-anthropic/gone"}, "", ErrSavedModelGone},
		{"pin gone", CopilotChoice{Pin: "gone"}, "", ErrSavedModelGone},
	} {
		got, err := r.Select(tc.c)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
	if _, err := (CopilotRegistry{}).Select(CopilotChoice{}); !errors.Is(err, ErrNoModels) {
		t.Errorf("empty registry err = %v, want ErrNoModels", err)
	}
}

func TestRuntimeDirRejectsUnsafeDirs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux runtime dir rules")
	}
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	dir := filepath.Join(base, "claude-code", "copilot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CopilotRuntimeDir(); err == nil {
		t.Error("mode 0755 dir accepted")
	}
	os.RemoveAll(dir)
	target := t.TempDir()
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := CopilotRuntimeDir(); err == nil {
		t.Error("symlinked dir accepted")
	}
	os.Remove(dir)
	// a correct-mode dir on a non-tmpfs disk must still be refused (token never reaches disk)
	disk, err := os.MkdirTemp(".", "rt")
	if err != nil {
		t.Skipf("no writable disk dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(disk) })
	abs, _ := filepath.Abs(disk)
	if err := backingCheck(abs); err == nil {
		t.Skip("working dir is tmpfs here; the on-disk rejection cannot be exercised")
	}
	t.Setenv("XDG_RUNTIME_DIR", abs)
	if _, err := CopilotRuntimeDir(); err == nil {
		t.Error("0700 dir on disk accepted")
	}
}

func TestWriteRegistryModeAndKeep(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux runtime dir")
	}
	base := "/run/user/" + itoa(os.Getuid())
	if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
		t.Skip("no tmpfs /run/user/<uid>")
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	live, err := WriteCopilotRegistry(os.Getpid(), []byte("{}"))
	if err != nil {
		t.Skipf("runtime dir unusable here: %v", err)
	}
	t.Cleanup(func() { os.Remove(live) })
	if fi, _ := os.Stat(live); fi.Mode().Perm() != 0o600 {
		t.Errorf("registry mode %o, want 0600", fi.Mode().Perm())
	}
	dead := CopilotRegistryFile(filepath.Dir(live), 1<<22+12345) // above pid_max: never alive
	if err := os.WriteFile(dead, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if CopilotRegistryKeep(dead) {
		t.Errorf("dead pid file %s kept", dead)
	}
	if !CopilotRegistryKeep(live) {
		t.Errorf("live pid file %s swept", live)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestCopilotRegistryRefusedOnDarwin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := copilotGOOS
	t.Cleanup(func() { copilotGOOS = old })
	copilotGOOS = "darwin"

	_, err := PrepareCopilotHome(true)
	if err == nil || !strings.Contains(err.Error(), "--direct") || !strings.Contains(err.Error(), "follow-up") {
		t.Fatalf("PrepareCopilotHome on darwin: %v", err)
	}
	cr, err := ConfigureCopilotRegistry(context.Background(), nil, providers.Target{}, nil, true)
	if err == nil || cr.Token != "" {
		t.Fatalf("ConfigureCopilotRegistry on darwin: token %q err %v", cr.Token, err)
	}
	copilotGOOS = "linux"
	if err := checkCopilotRegistryOS(); err != nil {
		t.Fatalf("linux refused: %v", err)
	}
}

// --print-env (dry) reads state only: it neither starts the router nor creates the runtime dir or private home.
func TestCopilotRegistryDryCreatesNothing(t *testing.T) {
	cfg, run := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("HOME", t.TempDir())
	oldD, oldE, oldR := discoverModels, ensureRouter, runningRouter
	t.Cleanup(func() { discoverModels, ensureRouter, runningRouter = oldD, oldE, oldR })
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]CopilotEndpoint, error) {
		var ep CopilotEndpoint
		ep.ID = "databricks-claude-sonnet-5-5"
		return []CopilotEndpoint{ep}, nil
	}
	ensureRouter = func(context.Context) (string, error) { t.Fatal("dry ensured (may start) the router"); return "", nil }
	runningRouter = func(context.Context) (proxy.State, bool) { return proxy.State{}, false }

	tgt := providers.Target{Provider: &providers.Provider{Name: "dbx"}}
	cr, err := ConfigureCopilotRegistry(context.Background(), nil, tgt, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Token != "" || len(cr.Env) == 0 {
		t.Fatalf("dry launch: token %q env %v", cr.Token, cr.Env)
	}
	for _, d := range []string{cfg, run} {
		if ents, _ := os.ReadDir(d); len(ents) != 0 {
			t.Errorf("dry run created entries under %s: %v", d, ents)
		}
	}
}

// Concurrent launches initialise the shared private home without ENOENT/EEXIST aborts.
func TestPrepareCopilotHomeConcurrent(t *testing.T) {
	cfg, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("HOME", userHome)
	for _, n := range copilotSeedLinks {
		if err := os.MkdirAll(filepath.Join(userHome, ".copilot", n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			if _, err := PrepareCopilotHome(false); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if _, err := os.Readlink(filepath.Join(copilotRegistryHome(), copilotSeedLinks[0])); err != nil {
		t.Errorf("link missing after concurrent init: %v", err)
	}
}

// An explicit --model (consumed by ParseArgs, so absent from args) selects the registry model over the default.
func TestCopilotRegistryExplicitModelWins(t *testing.T) {
	t.Cleanup(WithCopilotOS("linux")) // the macOS refusal has its own test
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		eps := []map[string]any{}
		for _, n := range []string{"databricks-claude-sonnet-5-5", "databricks-claude-opus-5-5"} {
			eps = append(eps, map[string]any{"name": n, "task": "llm/v1/chat", "state": map[string]string{"ready": "READY"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoints": eps})
	}))
	t.Cleanup(srv.Close)
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport.(*http.Transport).Clone()
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server
	t.Cleanup(func() { http.DefaultTransport = old })

	row := `"dbx":{"preset":"databricks","host":"` + srv.URL + `","auth":{"type":"key","env":"TEST_GW_KEY"},` +
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5"}}`
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+row+`}}}`)
	t.Setenv("TEST_GW_KEY", "dbx-key")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	oldLook := lookPath
	t.Cleanup(func() { lookPath = oldLook })
	lookPath = func(string) (string, error) { return "/bin/copilot", nil }

	argvModel := func(model string) string {
		plan, err := LaunchPlan(context.Background(), "copilot", LaunchOptions{Provider: "dbx", Model: model, PrintEnv: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i, a := range plan.Argv {
			if a == "--model" && i+1 < len(plan.Argv) {
				return plan.Argv[i+1]
			}
		}
		t.Fatalf("no --model in %v", plan.Argv)
		return ""
	}
	def, flagged := argvModel(""), argvModel("claude-opus-5-5")
	if def == flagged || !strings.HasSuffix(flagged, "/claude-opus-5-5") {
		t.Fatalf("default %q, flagged %q: --model was ignored", def, flagged)
	}
}
