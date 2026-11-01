package providers

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func readOverlay(t *testing.T, key string) string {
	t.Helper()
	var s string
	if err := vault.View(func(tx *vault.Tx) error { s, _ = tx.Get(vault.Overlay, key); return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUpdateOverlayDocDryRunWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderModel("personal", "haiku", "seed-x"); err != nil {
		t.Fatal(err)
	}
	before, prevBefore := readOverlay(t, "providers"), readOverlay(t, "providers.prev")
	mutate := func(top map[string]json.RawMessage, _ *vault.Tx) error {
		top["marker"] = json.RawMessage(`"changed"`)
		return nil
	}
	oldDoc, newDoc, err := UpdateOverlayDoc(false, mutate)
	if err != nil {
		t.Fatal(err)
	}
	if string(oldDoc) != before || string(newDoc) == before || !json.Valid(newDoc) {
		t.Fatalf("dry run docs wrong: old=%q new=%q", oldDoc, newDoc)
	}
	if got := readOverlay(t, "providers"); got != before {
		t.Errorf("dry run wrote providers: %q", got)
	}
	if got := readOverlay(t, "providers.prev"); got != prevBefore {
		t.Errorf("dry run wrote providers.prev: %q", got)
	}

	if _, _, err := UpdateOverlayDoc(true, mutate); err != nil {
		t.Fatal(err)
	}
	if got := readOverlay(t, "providers.prev"); got != before {
		t.Errorf("apply: providers.prev = %q, want old doc", got)
	}
	if got := readOverlay(t, "providers"); got != string(newDoc) {
		t.Errorf("apply: providers = %q, want %q", got, newDoc)
	}
}

// TestSetProviderModelSeedsBuiltinEntry: a provider absent from the overlay keeps its built-in auth/route/opencodeID
// when one alias is changed (an overlay entry replaces the built-in whole, so a models-only entry would break it).
func TestSetProviderModelSeedsBuiltinEntry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderModel("personal", "haiku", "gpt-x-mini"); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers["personal"]
	if p.Models["haiku"] != "gpt-x-mini" || p.Models["opus"] == "" || p.OpencodeID != "github-copilot" || p.Auth.Login == "" {
		t.Errorf("entry lost built-in fields: %+v", p)
	}
}

func orderRow(n string) json.RawMessage {
	return json.RawMessage(`{"preset":"databricks","host":"https://` + n + `.cloud.databricks.com"}`)
}

// seedOrder writes the flat rows to the hermetic vault in the given declared order.
func seedOrder(t *testing.T, names ...string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, _, err := UpdateOverlayDoc(true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		m := map[string]json.RawMessage{}
		for _, n := range names {
			m[n] = orderRow(n)
		}
		b, err := MarshalOrdered(names, m)
		top["providers"] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func storedProfiles(t *testing.T) []string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readOverlay(t, "providers")), &top); err != nil {
		t.Fatal(err)
	}
	return profileOrder(top["providers"])
}

func touch(name string) func(map[string]json.RawMessage, *vault.Tx) error {
	return func(top map[string]json.RawMessage, _ *vault.Tx) error {
		var rows map[string]json.RawMessage
		if err := json.Unmarshal(top["providers"], &rows); err != nil {
			return err
		}
		var f map[string]json.RawMessage
		_ = json.Unmarshal(rows[name], &f)
		f["note"] = json.RawMessage(`"x"`)
		b, _ := json.Marshal(f)
		rows[name] = b
		// keep flat key order by re-marshalling in the current order
		keys := ObjectKeys(top["providers"])
		nb, err := MarshalOrdered(keys, rows)
		top["providers"] = nb
		return err
	}
}

func TestOverlayRoundTripKeepsDeclaredOrder(t *testing.T) {
	seedOrder(t, "dev", "qa", "prod")
	if got, want := storedProfiles(t), []string{"dev", "qa", "prod"}; !slices.Equal(got, want) {
		t.Fatalf("stored order = %v, want %v", got, want)
	}
	if _, _, err := UpdateOverlayDoc(true, func(map[string]json.RawMessage, *vault.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, want := storedProfiles(t), []string{"dev", "qa", "prod"}; !slices.Equal(got, want) {
		t.Errorf("after no-op update = %v, want %v", got, want)
	}
}

func TestOverlayUnrelatedUpdateDoesNotResort(t *testing.T) {
	seedOrder(t, "prod", "dev", "qa")
	if _, _, err := UpdateOverlayDoc(true, touch("dev")); err != nil {
		t.Fatal(err)
	}
	if got, want := storedProfiles(t), []string{"prod", "dev", "qa"}; !slices.Equal(got, want) {
		t.Errorf("after row update = %v, want declared %v (not sorted)", got, want)
	}
}

func TestOverlayNewRowAppendsLast(t *testing.T) {
	seedOrder(t, "prod", "dev", "qa")
	_, _, err := UpdateOverlayDoc(true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		var rows map[string]json.RawMessage
		if err := json.Unmarshal(top["providers"], &rows); err != nil {
			return err
		}
		rows["aaa"] = orderRow("aaa")
		keys := append(ObjectKeys(top["providers"]), "aaa")
		b, err := MarshalOrdered(keys, rows)
		top["providers"] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := storedProfiles(t), []string{"prod", "dev", "qa", "aaa"}; !slices.Equal(got, want) {
		t.Errorf("after add = %v, want new row last %v", got, want)
	}
}

func TestMarshalOrderedAndObjectKeys(t *testing.T) {
	m := map[string]json.RawMessage{"b": json.RawMessage(`1`), "a": json.RawMessage(`{"x":2}`), "c": json.RawMessage(`3`)}
	got, err := MarshalOrdered([]string{"c", "missing", "a", "b"}, m)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"c":3,"a":{"x":2},"b":1}` {
		t.Errorf("MarshalOrdered = %s", got)
	}
	if k := ObjectKeys(got); !slices.Equal(k, []string{"c", "a", "b"}) {
		t.Errorf("ObjectKeys = %v", k)
	}
	if e, _ := MarshalOrdered(nil, m); string(e) != `{}` {
		t.Errorf("empty keys = %s", e)
	}
	for _, bad := range []string{`[1,2]`, `"s"`, ``, `null`} {
		if k := ObjectKeys(json.RawMessage(bad)); len(k) != 0 {
			t.Errorf("ObjectKeys(%q) = %v, want none", bad, k)
		}
	}
}

func TestNestRowsOrderAndLeftovers(t *testing.T) {
	rows := map[string]rawRow{
		"prod": {provider: "databricks", raw: orderRow("prod")},
		"dev":  {provider: "databricks", raw: orderRow("dev")},
		"zz":   {provider: "databricks", raw: orderRow("zz")},
		"aa":   {provider: "databricks", raw: orderRow("aa")},
	}
	b, err := nestRows(rows, []string{"prod", "gone", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(b, &top)
	if got, want := ObjectKeys(top["databricks"]), []string{"prod", "dev", "aa", "zz"}; !slices.Equal(got, want) {
		t.Errorf("nestRows order = %v, want %v (listed order, then sorted leftovers)", got, want)
	}
}
