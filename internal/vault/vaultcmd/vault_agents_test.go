package vaultcmd

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// editRecords re-exports, lets edit change the "records" map inside the agents section, and returns the new doc bytes.
func editRecords(t *testing.T, raw []byte, edit func(map[string]any)) []byte {
	return editTestDoc(t, raw, func(d *Doc) {
		top := map[string]any{}
		if len(d.Agents) != 0 {
			if err := json.Unmarshal(d.Agents, &top); err != nil {
				t.Fatal(err)
			}
		}
		if top == nil {
			top = map[string]any{} // an empty section exports as null
		}
		recs, _ := top[recordsKey].(map[string]any)
		if recs == nil {
			recs = map[string]any{}
		}
		edit(recs)
		top[recordsKey] = recs
		d.Agents, _ = json.Marshal(top)
	})
}

// Records appear under agents.records beside the overlay, an unchanged Import writes nothing, and edits land in the records.
func TestExportImportAgentRecords(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{"launch":{"env":["X=1"]}}}}`)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "chatgpt", Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	orig := exportTestDoc(t)
	assertExportShowsRecords(t, orig)
	assertUnchangedImportIsNoop(t, orig)
	edited := editRecords(t, orig, func(recs map[string]any) {
		recs["codex"] = map[string]any{"profile": "openai", "model": "m1"}
		recs["copilot"] = map[string]any{"profile": "personal", "effort": "high"}
	})
	if err := Import(orig, edited); err != nil {
		t.Fatal(err)
	}
	assertRecordEditsLanded(t)
}

// assertExportShowsRecords fails unless the exported agents section holds the overlay plus the stored records.
func assertExportShowsRecords(t *testing.T, orig []byte) {
	t.Helper()
	var d Doc
	if err := json.Unmarshal(orig, &d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.Agents), `"records"`) || !strings.Contains(string(d.Agents), `"chatgpt"`) || !strings.Contains(string(d.Agents), `"launch"`) {
		t.Fatalf("agents section = %s, want the overlay plus records", d.Agents)
	}
}

// assertUnchangedImportIsNoop fails when importing the exported doc unchanged errors or writes the vault.
func assertUnchangedImportIsNoop(t *testing.T, orig []byte) {
	t.Helper()
	before := vaultTestState(t)
	if err := Import(orig, orig); err != nil || !reflect.DeepEqual(vaultTestState(t), before) {
		t.Fatalf("unchanged Import err=%v, vault changed=%v", err, !reflect.DeepEqual(vaultTestState(t), before))
	}
}

// assertRecordEditsLanded fails unless the edited records are stored and the overlay kept without records.
func assertRecordEditsLanded(t *testing.T) {
	t.Helper()
	if got := vault.GetAgentRecord("codex"); got.Profile != "openai" || got.Model != "m1" {
		t.Errorf("codex record = %+v, want profile openai", got)
	}
	if got := vault.GetAgentRecord("copilot"); got.Profile != "personal" || got.Effort != "high" {
		t.Errorf("copilot record = %+v, want the added record", got)
	}
	if raw := vaultTestState(t)[vault.Overlay]["agents"]; !bytes.Contains([]byte(raw), []byte(`"launch"`)) || strings.Contains(raw, "records") {
		t.Errorf("overlay agents = %s, want the overlay kept and no records", raw)
	}
}

// A record naming an unknown agent or an unknown field is rejected and nothing is written.
func TestImportAgentRecordsValidation(t *testing.T) {
	vaulttest.Use(t)
	orig := exportTestDoc(t)
	for name, edit := range map[string]func(map[string]any){
		"unknown agent": func(r map[string]any) { r["nope"] = map[string]any{"profile": "x"} },
		"unknown field": func(r map[string]any) { r["codex"] = map[string]any{"colour": "x"} },
	} {
		before := vaultTestState(t)
		if err := Import(orig, editRecords(t, orig, edit)); err == nil || !strings.Contains(err.Error(), "agents.records") {
			t.Errorf("%s: err = %v, want an agents.records error", name, err)
		}
		if !reflect.DeepEqual(vaultTestState(t), before) {
			t.Errorf("%s: a rejected Import wrote to the vault", name)
		}
	}
}
