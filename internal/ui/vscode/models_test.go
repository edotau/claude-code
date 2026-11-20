package vscode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// testBase stands in for the running proxy's base URL.
const testBase = "http://127.0.0.1:1"

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
	routerBase = func(context.Context) string { return testBase }
	t.Cleanup(func() { routerBase = orig })
}

const harnessGroup = `{"name": "Databricks %s", "vendor": "customendpoint", "apiKey": "${input:harnessProxyToken}",
     "models": [{"id": "m", "url": "http://127.0.0.1:7/p/%s/v1/messages"}]}`

// Look-alike: same name and key reference, but its models point at a real host, so it is the user's.
const lookalike = `{"name": "Databricks dev", "vendor": "customendpoint", "apiKey": "${input:harnessProxyToken}",
     "models": [{"id": "m", "url": "https://dev.example.com/p/dev/v1/messages"}]}`

const foreignGroup = `{"name": "Anthropic",   "vendor": "customendpoint", "apiKey": "${input:anthropicApiKey}",
     "models": [{"id": "claude-sonnet-4-6", "url": "https://api.anthropic.com/v1/messages"}]}`

func syncModelsFile(t *testing.T, content string, check bool) (string, mergeResult, []string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), modelsFileName)
	writeFile(t, dest, content)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	res, removed, err := syncModels(dest, check)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	return string(got), res, removed
}

// The merge removes only the groups it wrote, keeps every other entry's bytes, and --check reports without writing.
func TestSyncModelsRemovesOnlyHarnessGroups(t *testing.T) {
	in := "[\n  " + fmt.Sprintf(harnessGroup, "dev", "dev") + ",\n  " + foreignGroup + ",\n  " + lookalike + ",\n  " +
		fmt.Sprintf(harnessGroup, "prod", "prod") + "\n]\n"
	got, res, removed := syncModelsFile(t, in, true)
	if got != in || !res.Changed || strings.Join(removed, ",") != "Databricks dev,Databricks prod" {
		t.Fatalf("check: changed=%v removed=%v wrote=%v", res.Changed, removed, got != in)
	}
	got, res, _ = syncModelsFile(t, in, false)
	if !res.Changed || strings.Contains(got, "Databricks prod") || strings.Contains(got, "127.0.0.1") ||
		strings.Count(got, "Databricks dev") != 1 {
		t.Fatalf("harness groups left:\n%s", got)
	}
	for _, keep := range []string{foreignGroup, lookalike} {
		if !strings.Contains(got, keep) {
			t.Errorf("foreign entry not kept verbatim: %s\nin:\n%s", keep, got)
		}
	}
}

// A file with no harness groups is not rewritten.
func TestSyncModelsLeavesCleanFileAlone(t *testing.T) {
	in := "// mine\n[" + foreignGroup + ",   " + lookalike + "]"
	got, res, removed := syncModelsFile(t, in, false)
	if got != in || res.Changed || len(removed) != 0 || res.Backup != "" {
		t.Fatalf("clean file touched: changed=%v removed=%v\n%s", res.Changed, removed, got)
	}
}

// --print lists dev's three READY chat endpoints with the session base, whatever the chat models file holds.
func TestPrintModelsListsDevEndpoints(t *testing.T) {
	seedDatabricks(t)
	var buf bytes.Buffer
	if err := printModels(&buf); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Profiles []struct {
			Profile, Base string
			Models        []struct{ ID string }
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil || len(out.Profiles) != 1 {
		t.Fatalf("print = %s (%v)", buf.Bytes(), err)
	}
	p := out.Profiles[0]
	if p.Profile != "dev" || p.Base != testBase+"/p/dev" || len(p.Models) != 3 {
		t.Fatalf("profile = %+v, want dev at %s with 3 models", p, testBase+"/p/dev")
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
