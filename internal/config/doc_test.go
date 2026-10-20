package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func exportTestDoc(t *testing.T) []byte {
	t.Helper()
	raw, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func editTestDoc(t *testing.T, raw []byte, edit func(*Doc)) []byte {
	t.Helper()
	var d Doc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	edit(&d)
	edited, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}

func vaultTestState(t *testing.T) map[string]map[string]string {
	t.Helper()
	state := map[string]map[string]string{}
	if err := vault.View(func(tx *vault.Tx) error {
		for _, bucket := range []string{vault.Pins, vault.Env, vault.Overlay, vault.Meta, vault.Secrets} {
			state[bucket] = tx.All(bucket)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func seedTestDoc(t *testing.T) {
	t.Helper()
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "anthropic")
	vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "old-model")
	vaulttest.Put(t, vault.Env, "KEEP_ENV", "keep")
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"anthropic"}`)
	vaulttest.Put(t, vault.Overlay, "vendors", `{}`)
	vaulttest.Put(t, vault.Overlay, "providers.prev", `{"sentinel":1}`)
	vaulttest.Put(t, vault.Meta, "key_command", `["pass","show","x"]`)
	vaulttest.Secret(t, "TAVILY_API_KEY", "tvly-dev-0123456789abcdef")
}

func TestExportEmpty(t *testing.T) {
	const want = "{\n  \"pins\": {},\n  \"env\": {},\n  \"providers\": null,\n  \"vendors\": null,\n  \"vault\": {\n    \"key_command\": \"\"\n  }\n}\n"
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
	vaulttest.Secret(t, "TAVILY_API_KEY", "tvly-dev-0123456789abcdef")
	vaulttest.Put(t, vault.Overlay, "providers.prev", `{"sentinel":1}`)
	raw := exportTestDoc(t)
	for _, absent := range []string{"TAVILY_API_KEY", "tvly-dev-0123456789abcdef", "providers.prev", "sentinel"} {
		t.Run(absent, func(t *testing.T) {
			if bytes.Contains(raw, []byte(absent)) {
				t.Errorf("Export contains %q", absent)
			}
		})
	}
}

func TestImportRoundTrip(t *testing.T) {
	vaulttest.Use(t)
	seedTestDoc(t)
	orig := exportTestDoc(t)
	before := vaultTestState(t)
	if err := Import(orig, orig); err != nil {
		t.Fatal(err)
	}
	if after := vaultTestState(t); !reflect.DeepEqual(after, before) {
		t.Fatal("unchanged Import modified a vault bucket")
	}
	if after := exportTestDoc(t); !bytes.Equal(after, orig) {
		t.Fatalf("round trip changed Export: %s", after)
	}
}

func TestImportValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Doc)
		raw  func([]byte) []byte
	}{
		{name: "bad pin name", edit: func(d *Doc) { d.Pins["foo-bar"] = "x" }},
		{name: "bad env name", edit: func(d *Doc) { d.Env["foo-bar"] = "x" }},
		{name: "multiline env", edit: func(d *Doc) { d.Env["X"] = "one\ntwo" }},
		{name: "carriage return env", edit: func(d *Doc) { d.Env["X"] = "one\rtwo" }},
		{name: "multiline pin", edit: func(d *Doc) { d.Pins["X"] = "one\ntwo" }},
		{name: "empty env", edit: func(d *Doc) { d.Env["X"] = "" }},
		{name: "empty pin", edit: func(d *Doc) { d.Pins["X"] = "" }},
		{name: "unknown field", raw: func(b []byte) []byte {
			return bytes.Replace(b, []byte("{"), []byte(`{"unknown":true,`), 1)
		}},
		{name: "trailing object", raw: func(b []byte) []byte { return append(b, []byte(" {}")...) }},
		{name: "trailing junk", raw: func(b []byte) []byte { return append(b, []byte(" junk")...) }},
		{name: "malformed JSON", raw: func([]byte) []byte { return []byte("{") }},
		{name: "providers", edit: func(d *Doc) {
			d.Providers = json.RawMessage(`{"providers":{"x":{"base_url":"nope"}}}`)
		}},
		{name: "vendors", edit: func(d *Doc) {
			d.Vendors = json.RawMessage(`{"vendors":{"codex":{"launch":{"args":42}}}}`)
		}},
		{name: "key command not array", edit: func(d *Doc) { d.Vault.KeyCommand = `"pass show x"` }},
		{name: "key command empty array", edit: func(d *Doc) { d.Vault.KeyCommand = `[]` }},
		{name: "key command empty executable", edit: func(d *Doc) { d.Vault.KeyCommand = `[""]` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			before := vaultTestState(t)
			edited := editTestDoc(t, orig, func(d *Doc) {
				d.Pins["HARNESS_PROVIDER"] = "openai"
				d.Env["NEW_ENV"] = "must-not-land"
				if tt.edit != nil {
					tt.edit(d)
				}
			})
			if tt.raw != nil {
				edited = tt.raw(edited)
			}
			if err := Import(orig, edited); err == nil {
				t.Error("Import accepted invalid document")
			}
			if got := exportTestDoc(t); !bytes.Equal(got, orig) {
				t.Error("invalid Import changed exported config")
			}
			if got := vaultTestState(t); !reflect.DeepEqual(got, before) {
				t.Error("invalid Import changed a vault bucket")
			}
		})
	}
}

func TestImportWrites(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*Doc)
		bucket string
		key    string
		want   string
	}{
		{"change pin", func(d *Doc) { d.Pins["HARNESS_PROVIDER"] = "openai" }, vault.Pins, "HARNESS_PROVIDER", "openai"},
		{"delete pin", func(d *Doc) { delete(d.Pins, "HARNESS_OPUS_MODEL") }, vault.Pins, "HARNESS_OPUS_MODEL", ""},
		{"add env", func(d *Doc) { d.Env["NEW_ENV"] = "new" }, vault.Env, "NEW_ENV", "new"},
		{"set providers", func(d *Doc) { d.Providers = json.RawMessage(`{"default":"openai"}`) },
			vault.Overlay, "providers", "{\n  \"default\": \"openai\"\n}\n"},
		{"remove vendors", func(d *Doc) { d.Vendors = json.RawMessage("null") }, vault.Overlay, "vendors", ""},
		{"key command", func(d *Doc) { d.Vault.KeyCommand = `["pass","show","y"]` },
			vault.Meta, "key_command", `["pass","show","y"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			want := vaultTestState(t)
			if tt.bucket == vault.Overlay && tt.key == "providers" {
				want[vault.Overlay]["providers.prev"] = want[vault.Overlay]["providers"]
			}
			if tt.want == "" {
				delete(want[tt.bucket], tt.key)
			} else {
				want[tt.bucket][tt.key] = tt.want
			}
			if err := Import(orig, editTestDoc(t, orig, tt.edit)); err != nil {
				t.Fatal(err)
			}
			if got := vaultTestState(t); !reflect.DeepEqual(got, want) {
				t.Fatalf("Import did not apply exactly the expected %s/%s change", tt.bucket, tt.key)
			}
		})
	}
}

func TestImportConflict(t *testing.T) {
	vaulttest.Use(t)
	seedTestDoc(t)
	orig := exportTestDoc(t)
	edited := editTestDoc(t, orig, func(d *Doc) {
		d.Pins["HARNESS_PROVIDER"] = "openai"
		d.Env["NEW_ENV"] = "must-not-land"
		d.Providers = json.RawMessage(`{"default":"openai"}`)
		d.Vendors = json.RawMessage("null")
		d.Vault.KeyCommand = `["pass","show","y"]`
	})
	vaulttest.Put(t, vault.Pins, "HARNESS_OPUS_MODEL", "concurrent-model")
	before := vaultTestState(t)
	if err := Import(orig, edited); !errors.Is(err, ErrChanged) {
		t.Errorf("Import conflict = %v, want ErrChanged", err)
	}
	if after := vaultTestState(t); !reflect.DeepEqual(after, before) {
		t.Error("conflicting Import wrote edited data to the vault")
	}
	events, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("conflicting Import appended %d audit events", len(events))
	}
}

func TestImportAudit(t *testing.T) {
	tests := []struct {
		name, key, value, resource string
		action                     audit.Action
	}{
		{"opus", "HARNESS_OPUS_MODEL", "new-model", "HARNESS_OPUS_MODEL", audit.ModelPin},
		{"sonnet", "HARNESS_SONNET_MODEL", "new-model", "HARNESS_SONNET_MODEL", audit.ModelPin},
		{"haiku", "HARNESS_HAIKU_MODEL", "new-model", "HARNESS_HAIKU_MODEL", audit.ModelPin},
		{"provider", "HARNESS_PROVIDER", "openai", "openai", audit.ProviderSwitch},
		{"unchanged", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			edited := orig
			if tt.key != "" {
				edited = editTestDoc(t, orig, func(d *Doc) { d.Pins[tt.key] = tt.value })
			}
			if err := Import(orig, edited); err != nil {
				t.Fatal(err)
			}
			events, err := audit.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if tt.key == "" {
				if len(events) != 0 {
					t.Fatalf("unchanged Import appended %d events", len(events))
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("Import appended %d events, want 1", len(events))
			}
			if e := events[0]; e.Action != tt.action || e.Resource != tt.resource {
				t.Errorf("audit event = %+v, want %s / %s", e, tt.action, tt.resource)
			}
			if tt.action == audit.ModelPin && events[0].Detail != tt.value {
				t.Errorf("model audit detail = %q, want %q", events[0].Detail, tt.value)
			}
			saved := exportTestDoc(t)
			if err := Import(saved, saved); err != nil {
				t.Fatal(err)
			}
			after, err := audit.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, events) {
				t.Error("unchanged Import appended audit events")
			}
		})
	}
}
