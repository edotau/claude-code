package configure

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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
	vaulttest.Put(t, vault.Overlay, "agents", `{}`)
	vaulttest.Put(t, vault.Overlay, "providers.prev", `{"sentinel":1}`)
	vaulttest.Put(t, vault.Meta, "key_command", `["pass","show","x"]`)
	vaulttest.Secret(t, "TAVILY_API_KEY", "tvly-dev-0123456789abcdef")
}

func TestExportEmpty(t *testing.T) {
	const want = "{\n  \"pins\": {},\n  \"env\": {},\n  \"providers\": null,\n  \"agents\": null,\n  \"settings\": null,\n  \"vault\": {\n    \"key_command\": \"\"\n  }\n}\n"
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
		{name: "agents", edit: func(d *Doc) {
			d.Agents = json.RawMessage(`{"agents":{"codex":{"launch":{"args":42}}}}`)
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
		{"remove agents", func(d *Doc) { d.Agents = json.RawMessage("null") }, vault.Overlay, "agents", ""},
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

func TestImportSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings json.RawMessage
		wantErr  string // substring; empty means no error
	}{
		{"autoMode object", json.RawMessage(`{"autoMode":{"soft_deny":["$defaults"]}}`), ""},
		{"scalar catalog key", json.RawMessage(`{"theme":"dark"}`), ""},
		{"unknown key", json.RawMessage(`{"bogusKey":1}`), "settings"},
		{"autoMode wrong type", json.RawMessage(`{"autoMode":"x"}`), "settings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			before := vaultTestState(t)
			edited := editTestDoc(t, orig, func(d *Doc) { d.Settings = tt.settings })
			err := Import(orig, edited)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Import() = %v, want nil", err)
				}
				after := exportTestDoc(t)
				var got Doc
				if err := json.Unmarshal(after, &got); err != nil {
					t.Fatal(err)
				}
				var gotVal, wantVal any
				if err := json.Unmarshal(got.Settings, &gotVal); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(tt.settings, &wantVal); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotVal, wantVal) {
					t.Errorf("Export settings = %s, want %s", got.Settings, tt.settings)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Import() = %v, want error containing %q", err, tt.wantErr)
			}
			if got := exportTestDoc(t); !bytes.Equal(got, orig) {
				t.Error("invalid settings Import changed exported config")
			}
			if got := vaultTestState(t); !reflect.DeepEqual(got, before) {
				t.Error("invalid settings Import changed a vault bucket")
			}
		})
	}
}

func TestImportSettingsRemoval(t *testing.T) {
	vaulttest.Use(t)
	seedTestDoc(t)
	vaulttest.Put(t, vault.Overlay, "settings", `{"theme":"dark"}`)
	orig := exportTestDoc(t)
	edited := editTestDoc(t, orig, func(d *Doc) { d.Settings = json.RawMessage("null") })
	if err := Import(orig, edited); err != nil {
		t.Fatal(err)
	}
	state := vaultTestState(t)
	if _, ok := state[vault.Overlay]["settings"]; ok {
		t.Error("removing settings left the overlay entry present")
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
		d.Agents = json.RawMessage("null")
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
		{"opus", "OPUS_MODEL", "new-model", "claude.slots.opus", audit.ModelPin},
		{"sonnet", "SONNET_MODEL", "new-model", "claude.slots.sonnet", audit.ModelPin},
		{"haiku", "HAIKU_MODEL", "new-model", "claude.slots.haiku", audit.ModelPin},
		{"provider", "PROFILE", "openai", "openai", audit.ProviderSwitch},
		{"unchanged", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			edited := orig
			if tt.key != "" {
				edited = editTestDoc(t, orig, func(d *Doc) {
					rest, recs, err := splitAgentRecords(d.Agents)
					if err != nil {
						t.Fatal(err)
					}
					if recs == nil {
						recs = map[string]vault.AgentRecord{}
					}
					recs["claude"] = recs["claude"].WithPin(tt.key, tt.value)
					if d.Agents, err = mergeAgentRecords(rest, recs); err != nil {
						t.Fatal(err)
					}
				})
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

func TestHelperEditor(t *testing.T) {
	mode := os.Getenv("CONFIG_TEST_EDITOR")
	if mode == "" {
		return
	}
	path := os.Args[len(os.Args)-1]
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("editor file mode = %04o, want 0600", info.Mode().Perm())
	}
	if err := os.WriteFile(os.Getenv("CONFIG_TEST_EDITOR_SEEN"), []byte("seen"), 0600); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "noop":
		os.Exit(0)
	case "fail":
		os.Exit(3)
	case "retry":
		marker := os.Getenv("CONFIG_TEST_EDITOR_RETRY")
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(marker, []byte("retry"), 0600); err != nil {
				t.Fatal(err)
			}
			mode = "bad"
		} else if err != nil {
			t.Fatal(err)
		} else {
			mode = "pin"
		}
	case "race":
		if err := vault.Update(func(tx *vault.Tx) error {
			return tx.Put(vault.Pins, "HARNESS_OPUS_MODEL", "concurrent-model")
		}); err != nil {
			t.Fatal(err)
		}
	case "pin", "bad":
	default:
		t.Fatalf("unknown helper editor mode %q", mode)
	}
	raw := []byte("{")
	if mode != "bad" {
		// A retry must replace the invalid file rather than try to decode it.
		d := Doc{Pins: map[string]string{"HARNESS_PROVIDER": "openai"}, Env: map[string]string{}}
		raw, err = json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func setupTestEditor(t *testing.T, mode, env string, terminal bool, answer bool) (string, *int) {
	t.Helper()
	vaulttest.Use(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("CONFIG_TEST_EDITOR", mode)
	t.Setenv("CONFIG_TEST_EDITOR_SEEN", filepath.Join(tmp, "seen"))
	t.Setenv("CONFIG_TEST_EDITOR_RETRY", filepath.Join(tmp, "retry"))
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv(env, os.Args[0]+" -test.run=^TestHelperEditor$ --")
	if env == "VISUAL" {
		t.Setenv("EDITOR", filepath.Join(tmp, "must-not-run"))
	}
	oldTerminal, oldConfirmEdit := isTerminal, confirmEdit
	calls := new(int)
	isTerminal = func() bool { return terminal }
	confirmEdit = func(q string) bool {
		*calls++
		if q != "Re-edit?" {
			t.Errorf("confirmEdit prompt = %q, want Re-edit?", q)
		}
		return answer
	}
	t.Cleanup(func() {
		isTerminal, confirmEdit = oldTerminal, oldConfirmEdit
		matches, err := filepath.Glob(filepath.Join(tmp, "claude-code-config-*"))
		if err != nil {
			t.Error(err)
		}
		if len(matches) != 0 {
			t.Errorf("Edit leaked temp files: %v", matches)
		}
	})
	return tmp, calls
}

func TestEdit(t *testing.T) {
	tests := []struct {
		mode, env, stdout string
		wantErr           bool
	}{
		{"pin", "VISUAL", "saved\n", false},
		{"pin", "EDITOR", "saved\n", false},
		{"noop", "VISUAL", "no changes\n", false},
		{"bad", "EDITOR", "", true},
		{"race", "VISUAL", "", true},
		{"fail", "EDITOR", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+tt.env, func(t *testing.T) {
			tmp, calls := setupTestEditor(t, tt.mode, tt.env, false, false)
			before := exportTestDoc(t)
			var stdout, stderr bytes.Buffer
			err := Edit(&stdout, &stderr)
			if (err != nil) != tt.wantErr {
				t.Errorf("Edit() = %v, want error %v", err, tt.wantErr)
			}
			if stdout.String() != tt.stdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.stdout)
			}
			if stderr.Len() != 0 {
				t.Errorf("non-terminal Edit printed stderr: %q", stderr.String())
			}
			if *calls != 0 {
				t.Errorf("non-terminal Edit called confirmEdit %d times", *calls)
			}
			if seen, err := os.ReadFile(filepath.Join(tmp, "seen")); err != nil || string(seen) != "seen" {
				t.Fatalf("helper did not inspect the temp file: %q, %v", seen, err)
			}
			switch tt.mode {
			case "pin":
				if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_PROVIDER": "openai"}) {
					t.Errorf("saved pins = %v", pins)
				}
			case "race":
				if !errors.Is(err, ErrChanged) {
					t.Errorf("Edit() = %v, want ErrChanged", err)
				}
				if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_OPUS_MODEL": "concurrent-model"}) {
					t.Errorf("conflicting Edit wrote pins: %v", pins)
				}
			default:
				if after := exportTestDoc(t); !bytes.Equal(before, after) {
					t.Error("unsaved Edit changed config")
				}
			}
			if tt.mode == "fail" && (err == nil || !strings.Contains(err.Error(), "editor "+os.Args[0]) || !strings.Contains(err.Error(), "exit status 3")) {
				t.Errorf("editor failure did not name editor and exit status: %v", err)
			}
		})
	}
}

func TestEditReedit(t *testing.T) {
	tests := []struct {
		name, mode string
		answer     bool
		calls      int
		saved      bool
	}{
		{"accept", "retry", true, 1, true},
		{"decline", "bad", false, 1, false},
		{"conflict", "race", true, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, calls := setupTestEditor(t, tt.mode, "VISUAL", true, tt.answer)
			before := exportTestDoc(t)
			var stdout, stderr bytes.Buffer
			err := Edit(&stdout, &stderr)
			if *calls != tt.calls {
				t.Errorf("confirmEdit called %d times, want %d", *calls, tt.calls)
			}
			if tt.saved {
				if err != nil || stdout.String() != "saved\n" {
					t.Errorf("re-edit = %v, stdout %q", err, stdout.String())
				}
				if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_PROVIDER": "openai"}) {
					t.Errorf("re-edit saved pins = %v", pins)
				}
			} else {
				if err == nil || stdout.Len() != 0 {
					t.Errorf("unsaved re-edit = %v, stdout %q", err, stdout.String())
				}
				if tt.mode == "race" {
					if !errors.Is(err, ErrChanged) {
						t.Errorf("re-edit = %v, want ErrChanged", err)
					}
					if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_OPUS_MODEL": "concurrent-model"}) {
						t.Errorf("conflicting re-edit wrote pins: %v", pins)
					}
				} else if after := exportTestDoc(t); !bytes.Equal(after, before) {
					t.Error("declined re-edit changed config")
				}
			}
			if tt.calls > 0 && !strings.Contains(stderr.String(), "config:") {
				t.Errorf("missing validation diagnostic: %q", stderr.String())
			}
			if tt.calls == 0 && stderr.Len() != 0 {
				t.Errorf("conflict printed stderr: %q", stderr.String())
			}
		})
	}
}
