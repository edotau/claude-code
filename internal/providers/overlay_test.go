package providers

import (
	"encoding/json"
	"slices"
	"strings"
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
	var newDoc []byte
	t.Run("dry run", func(t *testing.T) {
		var oldDoc []byte
		var err error
		oldDoc, newDoc, err = UpdateOverlayDoc(false, mutate)
		if err != nil {
			t.Fatal(err)
		}
		if string(oldDoc) != before || string(newDoc) == before || !json.Valid(newDoc) {
			t.Fatalf("dry run docs wrong: old=%q new=%q", oldDoc, newDoc)
		}
		wantOverlay(t, "providers", before, "dry run wrote providers: %q")
		wantOverlay(t, "providers.prev", prevBefore, "dry run wrote providers.prev: %q")
	})
	t.Run("apply", func(t *testing.T) {
		if _, _, err := UpdateOverlayDoc(true, mutate); err != nil {
			t.Fatal(err)
		}
		wantOverlay(t, "providers.prev", before, "apply: providers.prev = %q, want old doc")
		wantOverlay(t, "providers", string(newDoc), "apply: providers = %q, want the new doc")
	})
}

// wantOverlay fails with format (one %q for the stored value) unless the overlay key holds want.
func wantOverlay(t *testing.T, key, want, format string) {
	t.Helper()
	if got := readOverlay(t, key); got != want {
		t.Errorf(format, got)
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

// Writes through the real edit paths keep the stored declared order: no re-sort on a no-op or row edit, a new row last.
func TestOverlayWritesKeepDeclaredOrder(t *testing.T) {
	seedOrder(t, "prod", "dev", "qa")
	steps := []struct {
		name  string
		write func() error
		want  []string
	}{
		{"no-op update", func() error {
			_, _, err := UpdateOverlayDoc(true, func(map[string]json.RawMessage, *vault.Tx) error { return nil })
			return err
		}, []string{"prod", "dev", "qa"}},
		{"row edit", func() error { return SetProviderModel("dev", "opus", "my-opus") }, []string{"prod", "dev", "qa"}},
		{"new row", func() error {
			return SetProviderFields("aaa", map[string]any{"preset": "databricks", "host": "https://aaa.cloud.databricks.com"})
		}, []string{"prod", "dev", "qa", "aaa"}},
	}
	for _, st := range steps {
		if err := st.write(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got := storedProfiles(t); !slices.Equal(got, st.want) {
			t.Fatalf("after %s: stored order = %v, want %v", st.name, got, st.want)
		}
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

func TestUnsupportedAuthTypeWriteRefused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := SetProviderFields("dbx-bad", map[string]any{
		"protocol": "anthropic", "base_url": "https://example.cloud.databricks.com/serving-endpoints/anthropic",
		"auth": map[string]any{"type": "external-browser"},
	})
	if err == nil || !strings.Contains(err.Error(), `auth.type "external-browser" unknown`) {
		t.Fatalf("write err = %v", err)
	}
	reg, err := Load()
	if err != nil || reg.Providers["dbx-bad"] != nil {
		t.Errorf("rejected row landed: err=%v", err)
	}
}
