package vaultdoc

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
	if _, err := importDoc(orig, orig); err != nil {
		t.Fatal(err)
	}
	if after := vaultTestState(t); !reflect.DeepEqual(after, before) {
		t.Fatal("unchanged Import modified a vault bucket")
	}
	if after := exportTestDoc(t); !bytes.Equal(after, orig) {
		t.Fatalf("round trip changed Export: %s", after)
	}
}

// importValidationCase is one invalid part (edit the doc, then raw the bytes); path is its warning, "" = doc rejected.
type importValidationCase struct {
	name, path string
	edit       func(*Doc)
	raw        func([]byte) []byte
}

var importValidationCases = []importValidationCase{
	{name: "bad pin name", path: "pins.foo-bar", edit: func(d *Doc) { d.Pins["foo-bar"] = "x" }},
	{name: "bad env name", path: "env.foo-bar", edit: func(d *Doc) { d.Env["foo-bar"] = "x" }},
	{name: "multiline env", path: "env.X", edit: func(d *Doc) { d.Env["X"] = "one\ntwo" }},
	{name: "carriage return env", path: "env.X", edit: func(d *Doc) { d.Env["X"] = "one\rtwo" }},
	{name: "multiline pin", path: "pins.X", edit: func(d *Doc) { d.Pins["X"] = "one\ntwo" }},
	{name: "empty env", path: "env.X", edit: func(d *Doc) { d.Env["X"] = "" }},
	{name: "empty pin", path: "pins.X", edit: func(d *Doc) { d.Pins["X"] = "" }},
	{name: "unknown field", path: "unknown", raw: func(b []byte) []byte {
		return bytes.Replace(b, []byte("{"), []byte(`{"unknown":true,`), 1)
	}},
	{name: "trailing object", raw: func(b []byte) []byte { return append(b, []byte(" {}")...) }},
	{name: "trailing junk", raw: func(b []byte) []byte { return append(b, []byte(" junk")...) }},
	{name: "malformed JSON", raw: func([]byte) []byte { return []byte("{") }},
	{name: "providers", path: "providers", edit: func(d *Doc) {
		d.Providers = json.RawMessage(`{"providers":{"x":{"base_url":"nope"}}}`)
	}},
	{name: "agents", path: "agents", edit: func(d *Doc) {
		d.Agents = json.RawMessage(`{"agents":{"codex":{"launch":{"args":42}}}}`)
	}},
	{name: "key command not array", path: "vault.key_command", edit: func(d *Doc) { d.Vault.KeyCommand = `"pass show x"` }},
	{name: "key command empty array", path: "vault.key_command", edit: func(d *Doc) { d.Vault.KeyCommand = `[]` }},
	{name: "key command empty executable", path: "vault.key_command", edit: func(d *Doc) { d.Vault.KeyCommand = `[""]` }},
}

// editedInvalid builds the edited bytes for tt: two valid changes plus the case's invalid part.
func editedInvalid(t *testing.T, orig []byte, tt importValidationCase) []byte {
	t.Helper()
	edited := editTestDoc(t, orig, func(d *Doc) {
		d.Pins["HARNESS_PROVIDER"] = "openai"
		d.Env["NEW_ENV"] = "lands"
		if tt.edit != nil {
			tt.edit(d)
		}
	})
	if tt.raw != nil {
		edited = tt.raw(edited)
	}
	return edited
}

// An invalid entry is skipped with a warning naming its path while valid edits land; an unparsable doc applies none.
func TestImportValidation(t *testing.T) {
	for _, tt := range importValidationCases {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			want := vaultTestState(t)
			warnings, err := importDoc(orig, editedInvalid(t, orig, tt))
			if tt.path == "" {
				if err == nil {
					t.Error("Import accepted a document that does not parse")
				}
			} else {
				assertSkippedPath(t, warnings, err, tt.path)
				want[vault.Pins]["HARNESS_PROVIDER"], want[vault.Env]["NEW_ENV"] = "openai", "lands"
			}
			if got := vaultTestState(t); !reflect.DeepEqual(got, want) {
				t.Errorf("vault = %v, want only the valid changes %v", got, want)
			}
		})
	}
}

// assertSkippedPath fails unless importDoc succeeded with exactly one warning, naming path.
func assertSkippedPath(t *testing.T, warnings []string, err error, path string) {
	t.Helper()
	if err != nil || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "unable to parse "+path+": ") {
		t.Errorf("Import = %q, %v; want one warning for %s", warnings, err, path)
	}
}

// Mixed valid and bad entries: valid ones save, bad ones keep their stored value and warn by path, never by value.
func TestImportPartial(t *testing.T) {
	vaulttest.Use(t)
	seedTestDoc(t)
	vaulttest.SetRecord(t, "codex", "model", "keep-model")
	orig := exportTestDoc(t)
	const sentinel = "sk-SENTINEL-0123456789"
	edited := editRecords(t, editTestDoc(t, orig, func(d *Doc) {
		d.Pins["HARNESS_PROVIDER"] = "openai"
		d.Env["NEW_ENV"] = "lands"
		d.Env["LEAKY"] = sentinel + "\nsecond line"
		delete(d.Env, "KEEP_ENV")
	}), func(r map[string]any) {
		r["codex"] = map[string]any{"model": "new-model", "colour": sentinel}
		r["claude"] = map[string]any{"effort": "high"}
		r["nope"] = map[string]any{"profile": sentinel}
	})
	edited = bytes.Replace(edited, []byte(`"env":{`),
		[]byte(`"schema":1,"$schema":"x","extra":"`+sentinel+`","env":{"KEEP_ENV":42,`), 1)
	var stdout, stderr bytes.Buffer
	if retry, err := importEdited(&stdout, &stderr, orig, edited); retry || err != nil || stdout.String() != "saved\n" {
		t.Fatalf("importEdited = %v, %v; stdout %q", retry, err, stdout.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	paths := []string{"agents.records.codex", "agents.records.nope", "env.KEEP_ENV", "env.LEAKY", "extra"}
	for _, path := range paths {
		if strings.Count(stderr.String(), "warning: unable to parse "+path+": ") != 1 {
			t.Errorf("stderr lacks one warning for %s: %q", path, stderr.String())
		}
	}
	if len(lines) != len(paths) || strings.Contains(stderr.String(), sentinel) {
		t.Errorf("stderr = %q, want %d warnings and no value", stderr.String(), len(paths))
	}
	assertPartialState(t)
}

// assertPartialState fails unless TestImportPartial's valid edits landed and each bad entry kept its stored value.
func assertPartialState(t *testing.T) {
	t.Helper()
	state := vaultTestState(t)
	if state[vault.Pins]["HARNESS_PROVIDER"] != "openai" || state[vault.Env]["NEW_ENV"] != "lands" {
		t.Errorf("valid edits missing: pins %v env %v", state[vault.Pins], state[vault.Env])
	}
	if _, ok := state[vault.Env]["LEAKY"]; ok || state[vault.Env]["KEEP_ENV"] != "keep" {
		t.Errorf("bad env entries were not skipped: %v", state[vault.Env])
	}
	if got := vault.GetAgentRecord("codex"); got.Model != "keep-model" {
		t.Errorf("codex record = %+v, want the stored one kept", got)
	}
	if got := vault.GetAgentRecord("claude"); got.Effort != "high" {
		t.Errorf("claude record = %+v, want the valid edit", got)
	}
}

// A doc's own "schema" stamp imports at or below this build's version; a newer one is refused with nothing saved.
func TestImportSchema(t *testing.T) {
	for _, tt := range []struct {
		name, stamp, wantErr, wantWarn string
	}{
		{"current", "1", "", ""},
		{"older", "0", "", ""},
		{"newer", "2", "schema 2 is newer than this build", ""},
		{"not a number", `"v2"`, "", "unable to parse schema: "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			edited := editTestDoc(t, orig, func(d *Doc) { d.Pins["HARNESS_PROVIDER"] = "openai" })
			edited = bytes.Replace(edited, []byte("{"), []byte(`{"schema":`+tt.stamp+`,`), 1)
			warnings, err := importDoc(orig, edited)
			wantPin, gotErr := "openai", ""
			if tt.wantErr != "" {
				wantPin = "anthropic"
			}
			if err != nil {
				gotErr = err.Error()
			}
			pin, got := vaultTestState(t)[vault.Pins]["HARNESS_PROVIDER"], strings.Join(warnings, "\n")
			if pin != wantPin || !strings.Contains(gotErr, tt.wantErr) || (tt.wantErr == "") != (gotErr == "") ||
				(tt.wantWarn == "") != (got == "") || !strings.HasPrefix(got, tt.wantWarn) {
				t.Fatalf("Import = %q, %q, pin %q; want error %q, warning %q", got, gotErr, pin, tt.wantErr, tt.wantWarn)
			}
		})
	}
}

// A document that is not JSON names the line and column of the fault and saves nothing.
func TestImportSyntaxErrorNamesLine(t *testing.T) {
	vaulttest.Use(t)
	seedTestDoc(t)
	orig := exportTestDoc(t)
	before := vaultTestState(t)
	_, err := importDoc(orig, []byte("{\n  \"pins\": {},\n  ,\n}\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid JSON at line 3, column 3") {
		t.Errorf("Import = %v, want the line and column", err)
	}
	if !reflect.DeepEqual(vaultTestState(t), before) {
		t.Error("a document that does not parse changed the vault")
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
			if _, err := importDoc(orig, editTestDoc(t, orig, tt.edit)); err != nil {
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
		{"autoMode object", json.RawMessage(`{"autoMode":{"soft_deny":["$defaults"]}}`), "settings: retired overlay"},
		{"scalar catalog key", json.RawMessage(`{"theme":"dark"}`), "settings: retired overlay"},
		{"not an object", json.RawMessage(`[1]`), "settings: retired overlay"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			seedTestDoc(t)
			orig := exportTestDoc(t)
			before := vaultTestState(t)
			edited := editTestDoc(t, orig, func(d *Doc) { d.Settings = tt.settings })
			_, err := importDoc(orig, edited)
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
	if _, err := importDoc(orig, edited); err != nil {
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
	if _, err := importDoc(orig, edited); !errors.Is(err, errChanged) {
		t.Errorf("Import conflict = %v, want errChanged", err)
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
		{"opus", "OPUS_MODEL", "new-model", "claude.aliases.opus", audit.ModelPin},
		{"sonnet", "SONNET_MODEL", "new-model", "claude.aliases.sonnet", audit.ModelPin},
		{"haiku", "HAIKU_MODEL", "new-model", "claude.aliases.haiku", audit.ModelPin},
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
				edited = editClaudePin(t, orig, tt.key, tt.value)
			}
			if _, err := importDoc(orig, edited); err != nil {
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
			assertSingleAuditEvent(t, events, tt.action, tt.resource, tt.value)
			assertReimportQuiet(t, events)
		})
	}
}

// assertSingleAuditEvent fails unless events is exactly one event with the wanted action and resource (and detail for pins).
func assertSingleAuditEvent(t *testing.T, events []audit.Event, action audit.Action, resource, value string) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("Import appended %d events, want 1", len(events))
	}
	if e := events[0]; e.Action != action || e.Resource != resource {
		t.Errorf("audit event = %+v, want %s / %s", e, action, resource)
	}
	if action == audit.ModelPin && events[0].Detail != value {
		t.Errorf("model audit detail = %q, want %q", events[0].Detail, value)
	}
}

// editClaudePin returns orig with the claude agent record's pin key set to value.
func editClaudePin(t *testing.T, orig []byte, key, value string) []byte {
	t.Helper()
	return editTestDoc(t, orig, func(d *Doc) {
		rest, recs, err := splitAgentRecords(d.Agents)
		if err != nil {
			t.Fatal(err)
		}
		if recs == nil {
			recs = map[string]vault.AgentRecord{}
		}
		recs["claude"] = recs["claude"].WithPin(key, value)
		if d.Agents, err = mergeAgentRecords(rest, recs); err != nil {
			t.Fatal(err)
		}
	})
}

// assertReimportQuiet re-imports the saved doc unchanged and fails if it appends audit events beyond events.
func assertReimportQuiet(t *testing.T, events []audit.Event) {
	t.Helper()
	saved := exportTestDoc(t)
	if _, err := importDoc(saved, saved); err != nil {
		t.Fatal(err)
	}
	after, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, events) {
		t.Error("unchanged Import appended audit events")
	}
}

// testEditor is the $VISUAL/$EDITOR fixture: CONFIG_TEST_EDITOR picks what it does to the file in its last argument.
const testEditor = `#!/bin/sh
for f; do :; done
case "$(ls -l "$f")" in
-rw-------*) ;;
*) echo "editor file mode is not 0600: $(ls -l "$f")" >&2; exit 4 ;;
esac
printf seen > "$CONFIG_TEST_EDITOR_SEEN"
mode=$CONFIG_TEST_EDITOR
case $mode in
noop) exit 0 ;;
fail) exit 3 ;;
retry)
	# A retry must replace the invalid file rather than try to decode it.
	if [ -e "$CONFIG_TEST_EDITOR_RETRY" ]; then mode=pin; else printf retry > "$CONFIG_TEST_EDITOR_RETRY"; mode=bad; fi ;;
race)
	: > "$CONFIG_TEST_EDITOR_RACE.request"
	while [ ! -e "$CONFIG_TEST_EDITOR_RACE.done" ]; do sleep 0.01; done ;;
pin | bad) ;;
*) echo "unknown editor mode $mode" >&2; exit 5 ;;
esac
if [ "$mode" = bad ]; then printf '{' > "$f"; else cat "$CONFIG_TEST_EDITOR_DOC" > "$f"; fi
`

// writeTestEditor installs the testEditor script and the valid doc it saves in dir, returning the script's path.
func writeTestEditor(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the editor fixture is a sh script")
	}
	raw, err := json.Marshal(Doc{Pins: map[string]string{"HARNESS_PROVIDER": "openai"}, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	doc, editor := filepath.Join(dir, "doc.json"), filepath.Join(dir, "editor")
	if err := os.WriteFile(doc, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(editor, []byte(testEditor), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_TEST_EDITOR_DOC", doc)
	return editor
}

// raceTestEditor changes the vault while the "race" editor has the file open: it answers the script's request file.
func raceTestEditor(t *testing.T, marker string) {
	t.Helper()
	t.Setenv("CONFIG_TEST_EDITOR_RACE", marker)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := os.Stat(marker + ".request"); err == nil {
				break
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
		if err := vault.Update(func(tx *vault.Tx) error {
			return tx.Put(vault.Pins, "HARNESS_OPUS_MODEL", "concurrent-model")
		}); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(marker+".done", nil, 0600); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
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
	t.Setenv(env, writeTestEditor(t, tmp)+" --")
	if mode == "race" {
		raceTestEditor(t, filepath.Join(tmp, "race"))
	}
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
			assertEditEffect(t, tt.mode, err, before, filepath.Join(tmp, "editor"))
		})
	}
}

// assertEditEffect checks what an Edit in the given helper mode did (or refused to do) to the vault.
func assertEditEffect(t *testing.T, mode string, err error, before []byte, editor string) {
	t.Helper()
	switch mode {
	case "pin":
		if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_PROVIDER": "openai"}) {
			t.Errorf("saved pins = %v", pins)
		}
	case "race":
		if !errors.Is(err, errChanged) {
			t.Errorf("Edit() = %v, want errChanged", err)
		}
		if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_OPUS_MODEL": "concurrent-model"}) {
			t.Errorf("conflicting Edit wrote pins: %v", pins)
		}
	default:
		if after := exportTestDoc(t); !bytes.Equal(before, after) {
			t.Error("unsaved Edit changed config")
		}
	}
	if mode == "fail" {
		assertEditorFailureNamed(t, err, editor)
	}
}

// assertEditorFailureNamed fails unless err names the editor binary and its exit status.
func assertEditorFailureNamed(t *testing.T, err error, editor string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "editor "+editor) || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("editor failure did not name editor and exit status: %v", err)
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
				assertReeditSaved(t, err, stdout.String())
			} else {
				assertReeditRefused(t, tt.mode, err, stdout.String(), before)
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

// assertReeditSaved fails unless the re-edit succeeded and stored the corrected pins.
func assertReeditSaved(t *testing.T, err error, stdout string) {
	t.Helper()
	if err != nil || stdout != "saved\n" {
		t.Errorf("re-edit = %v, stdout %q", err, stdout)
	}
	if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_PROVIDER": "openai"}) {
		t.Errorf("re-edit saved pins = %v", pins)
	}
}

// assertReeditRefused fails unless the re-edit errored without saving and left the vault as it should be.
func assertReeditRefused(t *testing.T, mode string, err error, stdout string, before []byte) {
	t.Helper()
	if err == nil || stdout != "" {
		t.Errorf("unsaved re-edit = %v, stdout %q", err, stdout)
	}
	if mode != "race" {
		if after := exportTestDoc(t); !bytes.Equal(after, before) {
			t.Error("declined re-edit changed config")
		}
		return
	}
	if !errors.Is(err, errChanged) {
		t.Errorf("re-edit = %v, want errChanged", err)
	}
	if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_OPUS_MODEL": "concurrent-model"}) {
		t.Errorf("conflicting re-edit wrote pins: %v", pins)
	}
}

func TestImportRejectsNullDocument(t *testing.T) {
	vaulttest.Use(t)
	seedTestDoc(t)
	vaulttest.Put(t, vault.Overlay, "settings", `{"theme":"dark"}`)
	vaulttest.SetRecord(t, "codex", "model", "keep-model")
	orig := exportTestDoc(t)
	before := vaultTestState(t)
	if _, err := importDoc(orig, []byte(" \nnull\t")); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Errorf("Import(null) = %v, want JSON object error", err)
	}
	if got := exportTestDoc(t); !bytes.Equal(got, orig) {
		t.Error("null document changed exported pins, env, records, overlays or key_command")
	}
	if got := vaultTestState(t); !reflect.DeepEqual(got, before) {
		t.Error("null document changed a vault bucket")
	}
}

// An unmigrated vault (schema 6, fallback "max") must save: importDoc's migrating write once read as "vault changed".
func TestEditMigratesBeforeExport(t *testing.T) {
	setupTestEditor(t, "pin", "VISUAL", false, false)
	if err := vault.Update(func(tx *vault.Tx) error {
		if err := tx.Put(vault.Overlay, "providers", `{"fallback":["max"]}`); err != nil {
			return err
		}
		return tx.SetSchema(6)
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Edit(&stdout, &stderr); err != nil {
		t.Fatalf("Edit() on an unmigrated vault = %v, want saved", err)
	}
	if got := vaultTestState(t)[vault.Meta]["schema"]; got != "7" {
		t.Errorf("schema after Edit = %q, want 7", got)
	}
}
