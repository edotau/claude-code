package copilotreg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func ep(id string) Endpoint {
	return Endpoint{ID: id, Name: id, Task: "llm/v1/chat", Ready: "READY", Tested: true}
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

func render(t *testing.T, r Registry, cred string) rendered {
	t.Helper()
	b, err := Render(r, cred)
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
	r := Build("dev", dbx(), []Endpoint{ep("databricks-claude-sonnet-5-5")}, "http://127.0.0.1:9/")
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
		m["maxOutputTokens"] != float64(MaxOutputTokens) || m["maxContextWindowTokens"].(float64) <= 0 {
		t.Fatalf("model = %v", m)
	}
}

func TestBuildRenderMixedWires(t *testing.T) {
	eps := []Endpoint{ep("databricks-claude-sonnet-5-5"), ep("databricks-gpt-5-5"), ep("databricks-meta-llama-3-3-70b-instruct")}
	out := render(t, Build("dev", dbx(), eps, "http://r"), "cct_x")
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
	if _, err := Render(Registry{}, "x"); err == nil {
		t.Fatal("empty registry rendered")
	}
}

func TestBuildDropsRepeatedCanonicalIDs(t *testing.T) {
	r := Build("dev", dbx(), []Endpoint{ep("databricks-claude-sonnet-5-5"), ep("claude-sonnet-5-5")}, "http://r")
	if len(r.Models) != 1 {
		t.Fatalf("models = %v, want the duplicate dropped", r.Models)
	}
}

func fixture() Registry {
	return Registry{Profile: "dev", Models: []Model{
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
		c    Choice
		want string
		err  error
	}{
		{"alias", Choice{Requested: []string{"sonnet"}}, "dev-anthropic/claude-sonnet-5-5", nil},
		{"ref", Choice{Requested: []string{"dev-responses/gpt-5-5"}}, "dev-responses/gpt-5-5", nil},
		{"canonical", Choice{Requested: []string{"gpt-5-5"}}, "dev-responses/gpt-5-5", nil},
		{"wire", Choice{Requested: []string{"databricks-gpt-5-5"}}, "dev-responses/gpt-5-5", nil},
		{"pin", Choice{Pin: "gpt-5-5"}, "dev-responses/gpt-5-5", nil},
		{"saved", Choice{Saved: "dev-responses/gpt-5-5", Pin: "sonnet"}, "dev-responses/gpt-5-5", nil},
		{"requested beats saved", Choice{Requested: []string{"gpt-5-5"}, Saved: "sonnet"}, "dev-responses/gpt-5-5", nil},
		{"default", Choice{}, "dev-anthropic/claude-sonnet-5-5", nil},
		{"unknown", Choice{Requested: []string{"nope"}}, "", ErrUnknownModel},
		{"ambiguous", Choice{Requested: []string{"twin"}}, "", ErrAmbiguousModel},
		{"repeated", Choice{Requested: []string{"gpt-5-5", "sonnet"}}, "", ErrRepeatedSelection},
		{"saved gone", Choice{Saved: "dev-anthropic/gone"}, "", ErrSavedModelGone},
		{"pin gone", Choice{Pin: "gone"}, "", ErrSavedModelGone},
	} {
		got, err := r.Select(tc.c)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
	if _, err := (Registry{}).Select(Choice{}); !errors.Is(err, ErrNoModels) {
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
	if _, err := RuntimeDir(); err == nil {
		t.Error("mode 0755 dir accepted")
	}
	os.RemoveAll(dir)
	target := t.TempDir()
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeDir(); err == nil {
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
	if _, err := RuntimeDir(); err == nil {
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
	live, err := WriteRegistry(os.Getpid(), []byte("{}"))
	if err != nil {
		t.Skipf("runtime dir unusable here: %v", err)
	}
	t.Cleanup(func() { os.Remove(live) })
	if fi, _ := os.Stat(live); fi.Mode().Perm() != 0o600 {
		t.Errorf("registry mode %o, want 0600", fi.Mode().Perm())
	}
	dead := RegistryFile(filepath.Dir(live), 1<<22+12345) // above pid_max: never alive
	if err := os.WriteFile(dead, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Keep(dead) {
		t.Errorf("dead pid file %s kept", dead)
	}
	if !Keep(live) {
		t.Errorf("live pid file %s swept", live)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
