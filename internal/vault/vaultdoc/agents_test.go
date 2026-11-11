package vaultdoc

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/codex" // registers codex's declared extras for ValidateRecord
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

// Records appear under agents.records beside the overlay, an unchanged importDoc writes nothing, and edits land in the records.
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
	if _, err := importDoc(orig, edited); err != nil {
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
	if _, err := importDoc(orig, orig); err != nil || !reflect.DeepEqual(vaultTestState(t), before) {
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
		if _, err := importDoc(orig, editRecords(t, orig, edit)); err == nil || !strings.Contains(err.Error(), "agents.records") {
			t.Errorf("%s: err = %v, want an agents.records error", name, err)
		}
		if !reflect.DeepEqual(vaultTestState(t), before) {
			t.Errorf("%s: a rejected Import wrote to the vault", name)
		}
	}
}

// A record agent.ValidateRecord refuses warns by path without its value, keeps the stored record; valid edits apply.
func TestImportRefusesInvalidAgentRecord(t *testing.T) {
	key := "sk-" + strings.Repeat("a", 24)
	for name, c := range map[string]struct {
		agent string
		rec   map[string]any
	}{
		"effort":         {"codex", map[string]any{"effort": "turbo"}},
		"wrong agent":    {"codex", map[string]any{"account": "octo"}},
		"extras key":     {"codex", map[string]any{"extras": map[string]any{"nope": "x"}}},
		"extras value":   {"codex", map[string]any{"extras": map[string]any{"model_verbosity": "loud"}}},
		"launch.env":     {"codex", map[string]any{"launch": map[string]any{"env": []string{"OPENAI_API_KEY=" + key}}}},
		"launch.args":    {"codex", map[string]any{"launch": map[string]any{"args": []string{"--api-key", key}}}},
		"new bad record": {"gemini", map[string]any{"effort": "turbo"}},
	} {
		t.Run(name, func(t *testing.T) {
			vaulttest.Use(t)
			stored := vault.AgentRecord{Profile: "chatgpt", Model: "m1"}
			if err := vault.SetAgentRecord("codex", stored); err != nil {
				t.Fatal(err)
			}
			orig := exportTestDoc(t)
			warnings, err := importDoc(orig, editRecords(t, orig, func(recs map[string]any) {
				recs[c.agent] = c.rec
				recs["copilot"] = map[string]any{"effort": "high"}
			}))
			want := "unable to parse agents.records." + c.agent + ": "
			if err != nil || len(warnings) != 1 || !strings.HasPrefix(warnings[0], want) || strings.Contains(warnings[0], key) {
				t.Fatalf("Import = %q, %v; want one %q warning without the value", warnings, err, want)
			}
			var got map[string]vault.AgentRecord
			if err := vault.View(func(tx *vault.Tx) (err error) { got, err = tx.AgentRecords(); return err }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, map[string]vault.AgentRecord{"codex": stored, "copilot": {Effort: "high"}}) {
				t.Errorf("records = %+v, want codex kept as stored and copilot added", got)
			}
		})
	}
}
