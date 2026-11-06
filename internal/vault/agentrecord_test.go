package vault

import (
	"reflect"
	"strconv"
	"testing"
)

// legacyVault is a schema-3 vault holding pins, migrated by the next Update.
func legacyVault(t *testing.T, pins map[string]string) {
	t.Helper()
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error {
		for k, v := range pins {
			if err := tx.Put(Pins, k, v); err != nil {
				return err
			}
		}
		return tx.SetSchema(3)
	}); err != nil {
		t.Fatal(err)
	}
}

func migrate(t *testing.T) map[string]AgentRecord {
	t.Helper()
	if err := Update(func(*Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var recs map[string]AgentRecord
	if err := View(func(tx *Tx) (err error) { recs, err = tx.AgentRecords(); return err }); err != nil {
		t.Fatal(err)
	}
	return recs
}

// Given every pin shape in a schema-3 vault, the first Update folds them into records and deletes the old keys.
func TestMigrationFoldsEveryPinShapeIntoRecords(t *testing.T) {
	legacyVault(t, map[string]string{
		"CLAUDE_PROFILE": "max", "CLAUDE_OPUS_MODEL": "o-1", "CLAUDE_SONNET_MODEL": "s-1",
		"CODEX_PROFILE": "chatgpt", "CODEX_MODEL": "m1", "CODEX_EFFORT": "high",
		"COPILOT_PROFILE": "personal", "COPILOT_ACTIVE": "off", "COPILOT_GITHUB_USER": "alice", "COPILOT_CONTEXT": "1m",
		"GEMINI_PROFILE": "antigravity", "GEMINI_ACTIVE": "on",
		"OPENCODE_MODEL": "x", "HARNESS_KEEP": "stays",
	})
	got := migrate(t)
	want := map[string]AgentRecord{
		"claude":   {Profile: "max", Aliases: map[string]string{"opus": "o-1", "sonnet": "s-1"}},
		"codex":    {Profile: "chatgpt", Model: "m1", Effort: "high"},
		"copilot":  {Account: "alice", Context: "1m"}, // ACTIVE=off beats the PROFILE pin
		"gemini":   {Profile: "antigravity"},
		"opencode": {Profile: "zen-free", Model: "x"}, // unset and not off: its own login
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %+v\nwant      %+v", got, want)
	}
	var pins map[string]string
	var schema string
	if err := View(func(tx *Tx) error { pins = tx.All(Pins); schema, _ = tx.Get(Meta, "schema"); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pins, map[string]string{"HARNESS_KEEP": "stays"}) {
		t.Errorf("pins after migration = %v, want only HARNESS_KEEP", pins)
	}
	if schema != strconv.Itoa(schemaVersion) {
		t.Errorf("schema = %q, want %d", schema, schemaVersion)
	}
	if again := migrate(t); !reflect.DeepEqual(again, want) {
		t.Errorf("second run changed the records: %+v", again)
	}
}

// An unset, not-off agent migrates to its own-login profile (the old unset = on); ACTIVE=off and an own pin win.
func TestMigrationKeepsUnsetAgentsOnTheirOwnLogin(t *testing.T) {
	cases := []struct {
		name, agent string
		pins        map[string]string
		want        string
	}{
		{"codex unset", "codex", map[string]string{"CLAUDE_PROFILE": "dev"}, "chatgpt"},
		{"gemini unset", "gemini", map[string]string{"CLAUDE_PROFILE": "dev"}, "antigravity"},
		{"codex off stays off", "codex", map[string]string{"CLAUDE_PROFILE": "dev", "CODEX_ACTIVE": "off"}, ""},
		{"own pin wins", "codex", map[string]string{"CODEX_PROFILE": "dev"}, "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			legacyVault(t, c.pins)
			if got := migrate(t)[c.agent].Profile; got != c.want {
				t.Fatalf("%s profile = %q, want %q", c.agent, got, c.want)
			}
		})
	}
}

// After migration the legacy keys are gone, so clearing an agent's profile is not re-filled.
func TestClearedAgentProfileIsNotRefilled(t *testing.T) {
	legacyVault(t, map[string]string{"CLAUDE_PROFILE": "dev"})
	recs := migrate(t)
	recs["codex"] = AgentRecord{}
	if err := Update(func(tx *Tx) error { return tx.PutAgentRecords(recs) }); err != nil {
		t.Fatal(err)
	}
	var got map[string]AgentRecord
	if err := View(func(tx *Tx) (err error) { got, err = tx.AgentRecords(); return err }); err != nil {
		t.Fatal(err)
	}
	if got["codex"].Profile != "" || got["claude"].Profile != "dev" {
		t.Fatalf("records = %+v, want codex off and claude still dev", got)
	}
}

// A read before the migration sees the old pins as records, not every agent off.
func TestAgentRecordsFoldsUnmigratedPinsOnRead(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return tx.Put(Pins, "COPILOT_PROFILE", "dev") }); err != nil {
		t.Fatal(err)
	}
	var got map[string]AgentRecord
	if err := View(func(tx *Tx) (err error) { got, err = tx.AgentRecords(); return err }); err != nil {
		t.Fatal(err)
	}
	if got["copilot"].Profile != "dev" {
		t.Fatalf("copilot record = %+v, want profile dev from the raw pin", got["copilot"])
	}
}

// Given a schema-4 vault whose record stores "slots", the next Update rewrites it as "aliases" at schema 6.
func TestMigrationRenamesSlotsToAliases(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error {
		if err := tx.Put(Overlay, AgentRecordsKey, `{"claude":{"slots":{"opus":"x"}}}`); err != nil {
			return err
		}
		return tx.SetSchema(4)
	}); err != nil {
		t.Fatal(err)
	}
	got := migrate(t)
	if want := map[string]AgentRecord{"claude": {Aliases: map[string]string{"opus": "x"}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
	var raw, schema string
	_ = View(func(tx *Tx) error {
		raw, _ = tx.Get(Overlay, AgentRecordsKey)
		schema, _ = tx.Get(Meta, "schema")
		return nil
	})
	if raw != `{"claude":{"aliases":{"opus":"x"}}}` || schema != "6" {
		t.Errorf("stored = %s schema %s, want aliases key at schema 6", raw, schema)
	}
}

// Given a schema-5 vault with a "byok-models" overlay and a record pinned to model "byok", one Update moves the pool
// to "discovery-models" and the pin to "discovery"; a second Update changes nothing.
func TestMigrationRenamesByokToDiscovery(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error {
		if err := tx.Put(Overlay, "byok-models", `["m-a","m-b"]`); err != nil {
			return err
		}
		if err := tx.Put(Overlay, AgentRecordsKey, `{"opencode":{"profile":"p","model":"byok"},"codex":{"model":"gpt-x"}}`); err != nil {
			return err
		}
		return tx.SetSchema(5)
	}); err != nil {
		t.Fatal(err)
	}
	read := func() (pool, old, recs, schema string) {
		_ = View(func(tx *Tx) error {
			pool, _ = tx.Get(Overlay, PoolKey)
			old, _ = tx.Get(Overlay, "byok-models")
			recs, _ = tx.Get(Overlay, AgentRecordsKey)
			schema, _ = tx.Get(Meta, "schema")
			return nil
		})
		return
	}
	migrate(t)
	pool, old, recs, schema := read()
	if pool != `["m-a","m-b"]` || old != "" || schema != "6" ||
		recs != `{"codex":{"model":"gpt-x"},"opencode":{"profile":"p","model":"discovery"}}` {
		t.Fatalf("after migration: pool=%s old=%q recs=%s schema=%s", pool, old, recs, schema)
	}
	migrate(t)
	if p2, o2, r2, s2 := read(); p2 != pool || o2 != old || r2 != recs || s2 != schema {
		t.Errorf("second run changed the vault: %s %q %s %s", p2, o2, r2, s2)
	}
}

// Given a schema-5 vault, reads before any Update already see the "byok-models" pool and the "byok" pin as discovery.
func TestPreMigrationReadsMapByokToDiscovery(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error {
		_ = tx.Put(Overlay, "byok-models", `["m-a"]`)
		_ = tx.Put(Overlay, AgentRecordsKey, `{"opencode":{"model":"byok"}}`)
		return tx.SetSchema(5)
	}); err != nil {
		t.Fatal(err)
	}
	// Update just migrated; put the old layout back raw to model an unmigrated vault.
	if err := Update(func(tx *Tx) error {
		_ = tx.Put(Overlay, PoolKey, "")
		_ = tx.Put(Overlay, "byok-models", `["m-a"]`)
		_ = tx.Put(Overlay, AgentRecordsKey, `{"opencode":{"model":"byok"}}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var pool string
	var recs map[string]AgentRecord
	_ = View(func(tx *Tx) (err error) { pool, _ = tx.PoolOverlay(); recs, err = tx.AgentRecords(); return err })
	if pool != `["m-a"]` || recs["opencode"].Model != "discovery" {
		t.Fatalf("pool=%s model=%q, want the old data read as discovery", pool, recs["opencode"].Model)
	}
}
