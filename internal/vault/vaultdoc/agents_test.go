package vaultdoc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Records appear under agents.records beside the agents overlay in the exported doc.
func TestExportShowsAgentRecords(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["X=1"]}}}}`)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "chatgpt", Model: "m1"}); err != nil {
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
