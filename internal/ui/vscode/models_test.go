package vscode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// testBase stands in for the running proxy's base URL.
const testBase = "http://127.0.0.1:1"

// foreignProviders is a user's own entries in deliberately odd formatting: the merge must keep these bytes.
const foreignProviders = `[
  {"name": "Anthropic",   "vendor": "customendpoint", "apiKey": "${input:anthropicApiKey}",
     "models": [{"id": "claude-sonnet-4-6", "url": "https://api.anthropic.com/v1/messages"}]},
  {"name": "Databricks prod", "vendor": "customendpoint", "apiKey": "${input:harnessProxyToken}", "models": []},
  {"name": "Databricks gone", "vendor": "customendpoint", "apiKey": "${input:harnessProxyToken}", "models": []},
  "not-an-object"
]`

// seedDatabricks registers dev (signed in, cached listing), prod (signed in, cold) and qa (signed out).
func seedDatabricks(t *testing.T) {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("DEV_TOKEN", "dapi-dev-workspace-secret")
	t.Setenv("PROD_TOKEN", "dapi-prod-workspace-secret")
	row := func(name, env string) string {
		return `"` + name + `":{"preset":"databricks","host":"https://` + name + `.cloud.databricks.com",` +
			`"auth":{"type":"key","env":"` + env + `"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+row("dev", "DEV_TOKEN")+","+
		row("qa", "QA_TOKEN")+","+row("prod", "PROD_TOKEN")+`}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers["dev"]
	scope, err := serving.NewScope(p, "subject-dev")
	if err != nil {
		t.Fatal(err)
	}
	snap := serving.Snapshot{Schema: 1, Scope: scope, DiscoveredAt: time.Now().UTC(), Models: []providers.ModelInfo{
		{ID: "databricks-claude-opus-5-5", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-gpt-6-1-sol", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-qwen3-next-80b", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-gte-large-en", Task: "llm/v1/embeddings", Ready: "READY"},
		{ID: "databricks-llama-9", Task: "llm/v1/chat", Ready: "NOT_READY"},
	}}
	if err := serving.Save(p, snap); err != nil {
		t.Fatal(err)
	}
	orig := routerBase
	routerBase = func() string { return testBase }
	t.Cleanup(func() { routerBase = orig })
}

// theme vscode keeps foreign and cold groups verbatim, drops a gone one, adds dev's endpoints, writes no credential.
func TestThemeVSCodeWritesDatabricksModelGroups(t *testing.T) {
	hermeticVSCode(t)
	seedDatabricks(t)
	tmpl, dest := writeVSCodeFixture(t)
	models := filepath.Join(t.TempDir(), modelsFileName)
	writeFile(t, models, foreignProviders)
	args := append([]string{"--template", tmpl, "--dest", dest, "--models-dest", models}, isolationFlags(t)...)
	assertDryRun(t, args, models)
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return Run(args) }); code != 0 {
		t.Fatalf("apply exit %d", code)
	}
	got, _ := os.ReadFile(models)
	assertNoSecretAndForeignKept(t, got)
	assertGroups(t, got)
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return Run(append(args, "--check")) }); code != 0 {
		t.Errorf("--check after apply exit %d, want 0 (in sync)", code)
	}
}

// assertDryRun runs --dry-run: it names dev's group and notes cold prod (not signed-out qa), writing nothing.
func assertDryRun(t *testing.T, args []string, models string) {
	t.Helper()
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(append(args, "--dry-run")) })
	if code != 0 || !strings.Contains(stdout, "would merge chat models") || !strings.Contains(stdout, "Databricks dev (3 models)") {
		t.Fatalf("dry-run exit %d stdout %q", code, stdout)
	}
	if got, _ := os.ReadFile(models); string(got) != foreignProviders {
		t.Fatal("--dry-run wrote the chat models file")
	}
	if !strings.Contains(stderr, "prod has no cached listing") || strings.Contains(stderr, "qa") {
		t.Errorf("stderr %q, want one cold note for prod and none for signed-out qa", stderr)
	}
}

// assertNoSecretAndForeignKept fails on any workspace credential or proxy token, or a foreign line not kept verbatim.
func assertNoSecretAndForeignKept(t *testing.T, got []byte) {
	t.Helper()
	for _, secret := range []string{"dapi-dev-workspace-secret", "dapi-prod-workspace-secret", "cct_", "cck_"} {
		if bytes.Contains(got, []byte(secret)) {
			t.Fatalf("chat models file carries %q", secret)
		}
	}
	keep := strings.Split(foreignProviders, "\n")
	for _, line := range []string{keep[1], keep[2], keep[3], keep[5]} {
		if !bytes.Contains(got, []byte(strings.TrimSuffix(line, ","))) {
			t.Errorf("foreign bytes %q not kept verbatim in:\n%s", line, got)
		}
	}
}

// assertGroups checks the merged order and dev's three READY chat endpoints with their wire, URL and key reference.
func assertGroups(t *testing.T, raw []byte) {
	t.Helper()
	var groups []endpointGroup
	var all []json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("merged file is not a JSON array: %v\n%s", err, raw)
	}
	names := make([]string, len(all))
	for i, r := range all {
		var g endpointGroup
		_ = json.Unmarshal(r, &g) // "not-an-object" decodes to a zero group
		groups, names[i] = append(groups, g), g.Name
	}
	if strings.Join(names, ",") != "Anthropic,Databricks prod,,Databricks dev" {
		t.Fatalf("groups = %q, want foreign + cold prod in order, gone dropped, dev appended", names)
	}
	dev := groups[len(groups)-1]
	if dev.APIKey != proxyKeyRef || dev.Vendor != vendorEndpoint || len(dev.Models) != len(devWant) {
		t.Fatalf("dev group = %+v, want the key reference and %d models", dev, len(devWant))
	}
	for _, m := range dev.Models {
		assertModel(t, m)
	}
}

// devWant is each dev endpoint's apiType and URL.
var devWant = map[string][2]string{
	"databricks-claude-opus-5-5": {"messages", testBase + "/p/dev/v1/messages"},
	"databricks-gpt-6-1-sol":     {"responses", testBase + "/p/dev/responses/responses"},
	"databricks-qwen3-next-80b":  {"chat-completions", testBase + "/p/dev/openai/chat/completions"},
}

// assertModel checks one entry: full name as label, wire and URL, limits, and stateless only on Responses.
func assertModel(t *testing.T, m endpointModel) {
	t.Helper()
	w, ok := devWant[m.ID]
	if !ok || m.Name != m.ID || m.APIType != w[0] || m.URL != w[1] || m.ContextWindow <= 0 || m.MaxOutputTokens <= 0 ||
		(m.StatefulResponses != nil) != (w[0] == "responses") {
		t.Errorf("model %+v, want %v", m, w)
	}
}

// An endpoint the profile has no route for is left out of its group.
func TestProfileGroupDropsUnroutableEndpoint(t *testing.T) {
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks,
		Routes: map[string]string{providers.ProtocolResponses: "https://dev.cloud.databricks.com/serving-endpoints"}}
	g := profileGroup(p, []providers.ModelInfo{
		{ID: "databricks-gpt-6-1-sol", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-qwen3-next-80b", Task: "llm/v1/chat", Ready: "READY"},
	}, "http://127.0.0.1:1/p/dev")
	if len(g.Models) != 1 || g.Models[0].ID != "databricks-gpt-6-1-sol" {
		t.Fatalf("models = %+v, want only the Responses endpoint (no openai route for qwen)", g.Models)
	}
}
