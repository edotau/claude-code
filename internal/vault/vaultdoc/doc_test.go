package vaultdoc

import (
	"bytes"
	"testing"

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
