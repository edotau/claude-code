package configure

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

func TestMigrateVendorOverlay(t *testing.T) {
	for _, seed := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "populated"}[seed], func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
			if seed {
				err := vault.Update(func(tx *vault.Tx) error {
					return tx.Put(vault.Overlay, "vendors", `{"extra":"keep","vendors":{
						"opencode":{"mcp":{"allow":["fake"]},"permission":"dead","launch":{"args":["--fake"]}},
						"copilot":{"mcp":{"allow":["keep"]},"permission":"keep"}}}`)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(vault.Path())
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := MigrateVendorOverlay(true, &out); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(vault.Path())
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || (!seed && !os.IsNotExist(err)) {
				t.Fatal("dry-run wrote vault state")
			}
			if seed != strings.Contains(out.String(), "vault: dropped dead opencode mcp/permission overlay keys") {
				t.Error("dry-run migration notice does not match pending changes")
			}
			out.Reset()
			if err := MigrateVendorOverlay(false, &out); err != nil {
				t.Fatal(err)
			}
			if seed {
				if !strings.Contains(out.String(), "vault: dropped dead opencode mcp/permission overlay keys") {
					t.Error("migration omitted drop notice")
				}
				if err := vault.View(func(tx *vault.Tx) error {
					raw, _ := tx.Get(vault.Overlay, "vendors")
					var doc struct {
						Extra   string                                `json:"extra"`
						Vendors map[string]map[string]json.RawMessage `json:"vendors"`
					}
					if err := json.Unmarshal([]byte(raw), &doc); err != nil {
						return err
					}
					oc := doc.Vendors["opencode"]
					if oc["mcp"] != nil || oc["permission"] != nil || oc["launch"] == nil {
						t.Error("migration did not drop only opencode's dead fields")
					}
					if doc.Extra != "keep" || doc.Vendors["copilot"]["mcp"] == nil ||
						doc.Vendors["copilot"]["permission"] == nil {
						t.Error("migration changed unrelated overlay fields")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			var first string
			if err := vault.View(func(tx *vault.Tx) error {
				first, _ = tx.Get(vault.Overlay, "vendors")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := MigrateVendorOverlay(false, &out); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Error("idempotent migration printed a drop notice")
			}
			if err := vault.View(func(tx *vault.Tx) error {
				next, _ := tx.Get(vault.Overlay, "vendors")
				if next != first {
					t.Error("idempotent migration changed the overlay")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
