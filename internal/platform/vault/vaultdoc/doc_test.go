package vaultdoc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/route"
)

func exportTestDoc(t *testing.T) []byte {
	t.Helper()
	raw, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExportEmpty(t *testing.T) {
	const want = "{\n  \"env\": {},\n  \"providers\": null,\n  \"agents\": null,\n  \"vault\": {\n    \"key_command\": \"\"\n  }\n}\n"
	for _, created := range []bool{false, true} {
		name := "missing"
		if created {
			name = "created"
		}
		t.Run(name, func(t *testing.T) {
			vaulttest.Use(t)
			if created {
				if err := vault.Update(func(*vault.Tx) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if got := string(exportTestDoc(t)); got != want {
				t.Fatalf("Export() = %q, want %q", got, want)
			}
		})
	}
}

func TestExportOmitsSecretsAndPreviousProviders(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "TAVILY_API_KEY", "qZgeiabdpaaa")
	vaulttest.Put(t, vault.Overlay, "providers.prev", `{"sentinel":1}`)
	raw := exportTestDoc(t)
	for _, absent := range []string{"TAVILY_API_KEY", "qZgeiabdpaaa", "providers.prev", "sentinel"} {
		t.Run(absent, func(t *testing.T) {
			if bytes.Contains(raw, []byte(absent)) {
				t.Errorf("Export contains %q", absent)
			}
		})
	}
}

// Records appear under agents.records beside the agents overlay in the exported doc.
func TestExportShowsAgentRecords(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["X=1"]}}}}`)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Model: "m1", Route: route.Route{Profile: "chatgpt"}}); err != nil {
		t.Fatal(err)
	}
	var d Doc
	if err := json.Unmarshal(exportTestDoc(t), &d); err != nil {
		t.Fatal(err)
	}
	if s := string(d.Agents); !strings.Contains(s, `"records"`) || !strings.Contains(s, `"chatgpt"`) || !strings.Contains(s, `"launch"`) {
		t.Fatalf("agents section = %s, want the overlay plus records", d.Agents)
	}
}
