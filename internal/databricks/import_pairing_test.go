package databricks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

const pairingCfg = `[dev]
host = https://dev.example.cloud.databricks.com
auth_type = databricks-cli

[dev-sp]
host = https://dev.example.cloud.databricks.com
auth_type = oauth-m2m
client_id = dev-client
client_secret = test-only-sp-value
`

func TestImportPairedCredential(t *testing.T) {
	for _, seal := range []bool{false, true} {
		t.Run(map[bool]string{false: "without seal", true: "sealed"}[seal], func(t *testing.T) {
			useVaultTest(t)
			cfg := writeCfg(t, pairingCfg)
			args := []string{"--dbxcfg", cfg, "--apply"}
			if seal {
				args = append(args, "--seal-secrets")
			}
			if code, out, errs := captureOutput(t, func() int { return importCfg(append(slices.Clone(args), "--dry-run")) }); code != 0 || !strings.Contains(out, "dev <= SP dev-sp") {
				t.Fatalf("dry-run: %d %s %s", code, out, errs)
			}
			if reg, _ := providers.Load(); reg.Providers["dev"] != nil {
				t.Fatal("dry-run wrote a profile")
			}
			if len(secrets.Names()) != 0 {
				t.Fatal("dry-run sealed credentials")
			}
			code, out, errs := captureOutput(t, func() int { return importCfg(args) })
			if code != 0 || !strings.Contains(out, "dev <= SP dev-sp") || strings.Contains(out+errs, "test-only-sp-value") {
				t.Fatalf("import: %d %s %s", code, out, errs)
			}
			reg, err := providers.Load()
			if err != nil {
				t.Fatal(err)
			}
			if p := reg.Providers["dev"]; p == nil || p.Auth.Type != providers.AuthOAuth || p.Auth.SPClientID != "dev-client" {
				t.Fatalf("dev = %+v", p)
			}
			if reg.Providers["dev-sp"] != nil {
				t.Fatal("paired SP became a separate row")
			}
			if seal {
				val, source, err := secrets.Lookup("DATABRICKS_DEV_M2M")
				if err != nil || val != "test-only-sp-value" || source != "vault" {
					t.Fatalf("paired credential: %s %v", source, err)
				}
				if got := secrets.Names(); !slices.Equal(got, []string{"DATABRICKS_DEV_M2M"}) {
					t.Fatalf("sealed names = %v", got)
				}
			} else if len(secrets.Names()) != 0 {
				t.Fatal("sealed without --seal-secrets")
			}
			if code, _, _ := captureOutput(t, func() int { return importCfg(args) }); code != 0 {
				t.Fatal(code)
			}
			reg, _ = providers.Load()
			if reg.Providers["dev-sp"] != nil {
				t.Fatal("re-import added a paired SP row")
			}
		})
	}
}

func TestImportExplicitUnpairSurvivesReplace(t *testing.T) {
	useVaultTest(t)
	cfg := writeCfg(t, strings.Replace(pairingCfg, "[dev-sp]", "# claude-code: pair=none\n[dev-sp]", 1))
	args := []string{"--dbxcfg", cfg, "--apply"}
	if code, _, _ := captureOutput(t, func() int { return importCfg(args) }); code != 0 {
		t.Fatal(code)
	}
	cfg = writeCfg(t, pairingCfg)
	if code, _, _ := captureOutput(t, func() int { return importCfg([]string{"--dbxcfg", cfg, "--apply", "--replace"}) }); code != 0 {
		t.Fatal(code)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Providers["dev"].Auth.SPClientID != "" || reg.Providers["dev-sp"] == nil {
		t.Fatal("explicit unpair overwritten")
	}
}

func seedLegacyRows(t *testing.T) {
	t.Helper()
	for _, tag := range []string{"dev", "qa", "prod"} {
		fields := map[string]any{"preset": "databricks", "host": "https://" + tag + ".example.cloud.databricks.com", "auth": map[string]any{"type": "oauth"}}
		for _, id := range []string{tag, legacyUserPrefix + tag} {
			if err := providers.SetProviderFields(id, fields); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestImportFoldIsExplicitAndDryRun(t *testing.T) {
	useVaultTest(t)
	seedLegacyRows(t)
	cfg := writeCfg(t, "[dev]\nhost = https://dev.example.cloud.databricks.com\n")
	for _, flags := range [][]string{{"--apply"}, {"--fold-legacy", "--dry-run", "--apply"}} {
		code, out, errs := captureOutput(t, func() int { return importCfg(append([]string{"--dbxcfg", cfg}, flags...)) })
		if code != 0 {
			t.Fatalf("%v: %d %s %s", flags, code, out, errs)
		}
		reg, _ := providers.Load()
		if reg.Providers[legacyUserPrefix+"dev"] == nil {
			t.Fatalf("%v removed rows", flags)
		}
		if slices.Contains(flags, "--fold-legacy") && !strings.Contains(out, "fold "+legacyUserPrefix+"dev -> dev") {
			t.Fatalf("missing fold preview: %s", out)
		}
	}
	code, out, errs := captureOutput(t, func() int { return importCfg([]string{"--dbxcfg", cfg, "--fold-legacy", "--apply"}) })
	if code != 0 {
		t.Fatalf("fold: %d %s %s", code, out, errs)
	}
	reg, _ := providers.Load()
	for _, tag := range []string{"dev", "qa", "prod"} {
		if reg.Providers[legacyUserPrefix+tag] != nil || reg.Providers[tag] == nil {
			t.Fatalf("fold %s failed", tag)
		}
	}
}

func TestImportFoldReferenceVeto(t *testing.T) {
	for _, kind := range []string{"agent", "alias", "pin", "overlay", "plan", "custom plans", "host", "pairing"} {
		t.Run(kind, func(t *testing.T) {
			dir := useVaultTest(t)
			seedLegacyRows(t)
			old := legacyUserPrefix + "dev"
			var err error
			switch kind {
			case "agent", "alias", "pin", "overlay":
				err = vault.Update(func(tx *vault.Tx) error {
					switch kind {
					case "agent":
						return tx.Put(vault.Overlay, vault.AgentRecordsKey, `{"claude":{"profile":"`+old+`"}}`)
					case "alias":
						return tx.Put(vault.Overlay, "aliases", `{"dev":"`+old+`"}`)
					case "pin":
						return tx.Put(vault.Pins, "PROFILE", old)
					default:
						return tx.Put(vault.Overlay, "other", `{"profile":"`+old+`"}`)
					}
				})
			case "plan", "custom plans":
				sub := "plans"
				if kind == "custom plans" {
					sub = "custom"
					err = os.WriteFile(filepath.Join(dir, "settings.local.json"), []byte(`{"plansDirectory":"custom"}`), 0600)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err = os.MkdirAll(filepath.Join(dir, sub), 0700); err == nil {
					err = os.WriteFile(filepath.Join(dir, sub, "todo.md"), []byte("use "+old), 0600)
				}
			case "host":
				err = providers.SetProviderFields(old, map[string]any{"host": "https://other.example.cloud.databricks.com"})
			case "pairing":
				err = providers.SetProviderFields(old, map[string]any{"auth": map[string]any{"type": "oauth", "spClientId": "keep"}})
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg := writeCfg(t, "[dev]\nhost = https://dev.example.cloud.databricks.com\n")
			code, out, errs := captureOutput(t, func() int { return importCfg([]string{"--dbxcfg", cfg, "--fold-legacy", "--apply"}) })
			if code != 0 {
				t.Fatalf("fold: %d %s %s", code, out, errs)
			}
			reg, _ := providers.Load()
			if reg.Providers[old] == nil {
				t.Fatal("removed a referenced or incompatible row")
			}
			if reg.Providers[legacyUserPrefix+"qa"] != nil {
				t.Fatal("unrelated unreferenced row was not folded")
			}
		})
	}
}

func TestImportFoldFailsClosedOnUnreadablePlans(t *testing.T) {
	dir := useVaultTest(t)
	seedLegacyRows(t)
	plans := filepath.Join(dir, "plans")
	if err := os.MkdirAll(plans, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(plans, "broken.md")); err != nil {
		t.Fatal(err)
	}
	cfg := writeCfg(t, "[dev]\nhost = https://dev.example.cloud.databricks.com\n")
	code, _, _ := captureOutput(t, func() int { return importCfg([]string{"--dbxcfg", cfg, "--fold-legacy", "--apply"}) })
	if code == 0 {
		t.Fatal("fold ignored an unreadable plan")
	}
	reg, _ := providers.Load()
	if reg.Providers[legacyUserPrefix+"dev"] == nil {
		t.Fatal("fold deleted a row despite an incomplete reference scan")
	}
}
