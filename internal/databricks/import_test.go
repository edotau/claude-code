package databricks

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

const dbxCfgFixture = `[DEFAULT]
host = https://leak.example.cloud.databricks.com

[__settings__]
x = y

[alice-u2m]
host = https://alice.example.cloud.databricks.com
auth_type = databricks-cli

[svc-m2m]
host = https://svc.example.cloud.databricks.com
client_id = cid-1
client_secret = secret-value-1

[pat-one]
host = https://pat.example.cloud.databricks.com
token = dapi-token-value
`

func TestVaultImportDatabrickscfg(t *testing.T) {
	useVaultTest(t)
	cfg := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(cfg, []byte(dbxCfgFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--dbxcfg", cfg}
	checkVaultImportDryRun(t, base)
	checkVaultImportApply(t, base)
	code, out, _ := captureOutput(t, func() int { return importCfg(append(slices.Clone(base), "--apply")) })
	if code != 0 || !strings.Contains(out, "row exists") {
		t.Errorf("re-import should skip existing: %d %q", code, out)
	}
}

// checkVaultImportDryRun runs the default (dry-run) import and checks it prints no secret and writes no row.
func checkVaultImportDryRun(t *testing.T, base []string) {
	t.Helper()
	code, out, _ := captureOutput(t, func() int { return importCfg(base) })
	if code != 0 || !strings.Contains(out, "dry-run") || strings.Contains(out, "secret-value-1") {
		t.Fatalf("dry-run: %d %q", code, out)
	}
	if reg, _ := providers.Load(); reg != nil && reg.Providers["svc-m2m"] != nil {
		t.Fatal("dry-run wrote a row")
	}
}

// checkVaultImportApply applies with sealing and a rename, then checks rows, leaks and sealed secret names.
func checkVaultImportApply(t *testing.T, base []string) {
	t.Helper()
	args := append(slices.Clone(base), "--apply", "--seal-secrets", "--rename", "pat-one=pat-renamed")
	code, out, _ := captureOutput(t, func() int { return importCfg(args) })
	if code != 0 || strings.Contains(out, "secret-value-1") || strings.Contains(out, "dapi-token-value") {
		t.Fatalf("apply: %d %q", code, out)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice-u2m", "svc-m2m", "pat-renamed"} {
		if reg.Providers[id] == nil {
			t.Errorf("row %s missing", id)
		}
	}
	if reg.Providers["DEFAULT"] != nil || reg.Providers["__settings__"] != nil {
		t.Error("DEFAULT/__settings__ imported")
	}
	checkSealedNames(t, "DATABRICKS_SVC_M2M_M2M", "DATABRICKS_PAT_RENAMED_PAT")
}

// checkSealedNames fails for each wanted secret name the vault does not hold.
func checkSealedNames(t *testing.T, want ...string) {
	t.Helper()
	names := vault.Names()
	for _, n := range want {
		if !slices.Contains(names, n) {
			t.Errorf("secret %s not sealed (have %v)", n, names)
		}
	}
}

// writeCfg writes a databrickscfg fixture and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestImportCfgRefusals(t *testing.T) {
	const one = "[solo]\nhost = https://solo.example.cloud.databricks.com\nauth_type = databricks-cli\n"
	cases := []struct {
		name   string
		cfg    string
		args   []string
		want   string
		refuse bool // true: non-zero exit; false: a skipped row (exit 0, the note says why)
	}{
		{"old --profile flag", one, []string{"--apply", "--profile", "solo"}, "flag provided but not defined", true},
		{"empty config", "", []string{"--apply"}, "no profiles in", true},
		{"bad rename", one, []string{"--apply", "--rename", "solo"}, "want old=new", true},
		{"default only", "[DEFAULT]\nhost = https://d.example.cloud.databricks.com\n", []string{"--apply"},
			"never imported", false},
		{"bad id", one, []string{"--apply", "--rename", "solo=bad id!"}, "id must match", false},
		{"http host", "[web]\nhost = http://web.example.cloud.databricks.com\nauth_type = databricks-cli\n",
			[]string{"--apply"}, "no usable https host", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useVaultTest(t)
			args := append([]string{"--dbxcfg", writeCfg(t, c.cfg)}, c.args...)
			code, out, errOut := captureOutput(t, func() int { return importCfg(args) })
			if all := out + errOut; !strings.Contains(all, c.want) {
				t.Errorf("output %q lacks %q (code %d)", all, c.want, code)
			}
			if c.refuse == (code == 0) {
				t.Errorf("exit %d, refuse=%v", code, c.refuse)
			}
			reg, _ := providers.Load()
			for _, id := range []string{"solo", "web", "DEFAULT", "bad id!", "ghost"} {
				if reg != nil && reg.Providers[id] != nil {
					t.Errorf("row %q was written", id)
				}
			}
		})
	}
}

func TestImportAllSkippedWritesNothing(t *testing.T) {
	useVaultTest(t)
	cfg := writeCfg(t, "[bad name]\nhost = https://x.example.cloud.databricks.com\nauth_type = databricks-cli\n")
	code, out, _ := captureOutput(t, func() int { return importCfg([]string{"--dbxcfg", cfg, "--apply"}) })
	if code != 0 || !strings.Contains(out, "nothing to import") || strings.Contains(out, "wrote provider rows") {
		t.Fatalf("all-skipped apply: %d %q", code, out)
	}
	var doc string
	var found bool
	if err := vault.View(func(tx *vault.Tx) error { doc, found = tx.Get(vault.Overlay, "providers"); return nil }); err != nil {
		t.Logf("vault view: %v", err)
	}
	if found {
		t.Errorf("overlay written for an all-skipped import: %q", doc)
	}
}

func TestImportSealedPATIsFoundByCredentialForUnderscoreSuffixID(t *testing.T) {
	useVaultTest(t)
	cfg := filepath.Join(t.TempDir(), "cfg")
	body := "[dev_]\nhost = https://example.cloud.databricks.com\ntoken = dapi-e2e-value\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--dbxcfg", cfg, "--apply", "--seal-secrets"}
	if code, out, errs := captureOutput(t, func() int { return importCfg(args) }); code != 0 {
		t.Fatalf("import: %d %q %q", code, out, errs)
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers["dev_"] == nil {
		t.Fatalf("row dev_ missing: %v", err)
	}
	got, err := providers.Credential(context.Background(), reg.Providers["dev_"])
	if err != nil || got != "dapi-e2e-value" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
}

const gapCfg = `[DEFAULT]
host = https://leak.example.cloud.databricks.com
token = dapi-DEFAULT-LEAK

[svc-m2m]
host = https://svc.example.cloud.databricks.com
client_id = cid-1
client_secret = secret-value-1

[pat-one]
host = https://pat.example.cloud.databricks.com
token = dapi-token-value
`

func gapImport(t *testing.T, extra ...string) (code int, out string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(cfg, []byte(gapCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"--dbxcfg", cfg}, extra...)
	code, stdout, stderr := captureOutput(t, func() int { return importCfg(args) })
	return code, stdout + stderr
}

func gapProviderNames(t *testing.T) []string {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range reg.Providers {
		names = append(names, n)
	}
	return names
}

func TestVaultImportDryRunWithSealFlagWritesNothing(t *testing.T) {
	useVaultTest(t)
	before := gapProviderNames(t)
	code, out := gapImport(t, "--seal-secrets")
	if code != 0 || !strings.Contains(out, "dry-run") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if after := gapProviderNames(t); len(after) != len(before) || slices.Contains(after, "svc-m2m") {
		t.Errorf("dry-run created rows: %v", after)
	}
	for _, n := range vault.Names() {
		if strings.HasPrefix(n, "DATABRICKS_") {
			t.Errorf("dry-run sealed %s", n)
		}
	}
	for _, leak := range []string{"secret-value-1", "dapi-token-value", "dapi-DEFAULT-LEAK", "leak.example"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q", leak)
		}
	}
}

func TestVaultImportApplyWithoutSealStoresNoSecrets(t *testing.T) {
	useVaultTest(t)
	if code, out := gapImport(t, "--apply"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !slices.Contains(gapProviderNames(t), "svc-m2m") {
		t.Fatal("row not written")
	}
	for _, n := range vault.Names() {
		if strings.HasPrefix(n, "DATABRICKS_") {
			t.Errorf("sealed %s without --seal-secrets", n)
		}
	}
}

func TestVaultImportSealStoresReadableValuesAndNoDefaultLeak(t *testing.T) {
	useVaultTest(t)
	if code, out := gapImport(t, "--apply", "--seal-secrets"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for name, want := range map[string]string{
		"DATABRICKS_SVC_M2M_M2M": "secret-value-1",
		"DATABRICKS_PAT_ONE_PAT": "dapi-token-value",
	} {
		if got, src, err := vault.Lookup(name); err != nil || got != want || src != "vault" {
			t.Errorf("%s = %q (%s) %v", name, got, src, err)
		}
	}
	for _, n := range vault.Names() {
		if v, _, _ := vault.Lookup(n); v == "dapi-DEFAULT-LEAK" {
			t.Errorf("[DEFAULT] token sealed as %s", n)
		}
	}
}

func TestVaultImportExistingRowSkippedUnlessReplace(t *testing.T) {
	useVaultTest(t)
	if err := providers.SetProviderFields("pat-one", map[string]any{
		"protocol": "anthropic", "base_url": "https://old.example.cloud.databricks.com/serving-endpoints/anthropic",
		"auth": map[string]any{"type": "pat"},
	}); err != nil {
		t.Fatal(err)
	}
	host := func() string {
		reg, err := providers.Load()
		if err != nil {
			t.Fatal(err)
		}
		return reg.Providers["pat-one"].BaseURL
	}
	if code, out := gapImport(t, "--apply"); code != 0 || !strings.Contains(out, "row exists") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(host(), "old.example") {
		t.Errorf("existing row overwritten without --replace: %s", host())
	}
	if code, out := gapImport(t, "--apply", "--replace"); code != 0 {
		t.Fatalf("replace exit %d: %s", code, out)
	}
	if strings.Contains(host(), "old.example") {
		t.Errorf("--replace did not overwrite: %s", host())
	}
}

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
			checkPairedDryRun(t, args)
			checkPairedImport(t, args)
			checkPairedSealed(t, seal)
			if code, _, _ := captureOutput(t, func() int { return importCfg(args) }); code != 0 {
				t.Fatal(code)
			}
			reg, _ := providers.Load()
			if reg.Providers["dev-sp"] != nil {
				t.Fatal("re-import added a paired SP row")
			}
		})
	}
}

// checkPairedDryRun runs the import with --dry-run and checks it writes neither a profile nor a credential.
func checkPairedDryRun(t *testing.T, args []string) {
	t.Helper()
	if code, out, errs := captureOutput(t, func() int { return importCfg(append(slices.Clone(args), "--dry-run")) }); code != 0 || !strings.Contains(out, "dev <= SP dev-sp") {
		t.Fatalf("dry-run: %d %s %s", code, out, errs)
	}
	if reg, _ := providers.Load(); reg.Providers["dev"] != nil {
		t.Fatal("dry-run wrote a profile")
	}
	if len(vault.Names()) != 0 {
		t.Fatal("dry-run sealed credentials")
	}
}

// checkPairedImport runs the real import and checks the paired row, the leak-free output and no separate SP row.
func checkPairedImport(t *testing.T, args []string) {
	t.Helper()
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
}

// checkPairedSealed checks the paired credential is sealed under the user row only when --seal-secrets was set.
func checkPairedSealed(t *testing.T, seal bool) {
	t.Helper()
	if !seal {
		if len(vault.Names()) != 0 {
			t.Fatal("sealed without --seal-secrets")
		}
		return
	}
	val, source, err := vault.Lookup("DATABRICKS_DEV_M2M")
	if err != nil || val != "test-only-sp-value" || source != "vault" {
		t.Fatalf("paired credential: %s %v", source, err)
	}
	if got := vault.Names(); !slices.Equal(got, []string{"DATABRICKS_DEV_M2M"}) {
		t.Fatalf("sealed names = %v", got)
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
			if err := seedLegacyReference(t, dir, kind, old); err != nil {
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

// seedLegacyReference makes the legacy row old referenced (or incompatible) in the way kind names.
func seedLegacyReference(t *testing.T, dir, kind, old string) error {
	t.Helper()
	switch kind {
	case "agent", "alias", "pin", "overlay":
		return seedVaultReference(kind, old)
	case "plan", "custom plans":
		return seedPlanReference(t, dir, kind, old)
	case "host":
		return providers.SetProviderFields(old, map[string]any{"host": "https://other.example.cloud.databricks.com"})
	case "pairing":
		return providers.SetProviderFields(old, map[string]any{"auth": map[string]any{"type": "oauth", "spClientId": "keep"}})
	}
	return nil
}

// seedVaultReference stores a vault entry (agent record, alias, pin or other overlay) that names old.
func seedVaultReference(kind, old string) error {
	return vault.Update(func(tx *vault.Tx) error {
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
}

// seedPlanReference writes a plan file naming old, under the default plans dir or a plansDirectory setting.
func seedPlanReference(t *testing.T, dir, kind, old string) error {
	t.Helper()
	sub := "plans"
	if kind == "custom plans" {
		sub = "custom"
		if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), []byte(`{"plansDirectory":"custom"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sub, "todo.md"), []byte("use "+old), 0600)
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

func TestImportFoldSealedCredentials(t *testing.T) {
	for _, collision := range []string{"", providers.AuthOAuth, providers.AuthM2M, providers.AuthPAT} {
		t.Run("collision="+collision, func(t *testing.T) {
			useVaultTest(t)
			seedLegacyRows(t)
			old := legacyUserPrefix + "dev"
			for _, method := range []string{providers.AuthOAuth, providers.AuthM2M, providers.AuthPAT} {
				if err := vault.Store(secretName(old, method), "test-only-legacy-"+method); err != nil {
					t.Fatal(err)
				}
			}
			if collision != "" {
				if err := vault.Store(secretName("dev", collision), "test-only-destination"); err != nil {
					t.Fatal(err)
				}
			}
			before := foldSecretSnapshot(t)
			cfg := writeCfg(t, "[dev]\nhost = https://dev.example.cloud.databricks.com\n")
			args := []string{"--dbxcfg", cfg, "--fold-legacy", "--apply"}
			key := os.Getenv("HARNESS_VAULT_KEY")
			t.Setenv("HARNESS_VAULT_KEY", "invalid-key-must-not-be-read")
			runFoldSecretImport(t, append(slices.Clone(args), "--dry-run"))
			t.Setenv("HARNESS_VAULT_KEY", key)
			assertFoldSecretRow(t, old, true)
			if !reflect.DeepEqual(before, foldSecretSnapshot(t)) {
				t.Fatal("preview changed sealed records")
			}
			runFoldSecretImport(t, args)
			assertFoldSecretRow(t, old, collision != "")
			if collision != "" {
				if !reflect.DeepEqual(before, foldSecretSnapshot(t)) {
					t.Fatal("collision changed sealed records")
				}
				return
			}
			assertFoldSecretMigration(t, old, before)
		})
	}
	t.Run("no credentials needs no key", func(t *testing.T) {
		useVaultTest(t)
		seedLegacyRows(t)
		t.Setenv("HARNESS_VAULT_KEY", "invalid-key-must-not-be-read")
		cfg := writeCfg(t, "[dev]\nhost = https://dev.example.cloud.databricks.com\n")
		runFoldSecretImport(t, []string{"--dbxcfg", cfg, "--fold-legacy", "--apply"})
		assertFoldSecretRow(t, legacyUserPrefix+"dev", false)
	})
}

func foldSecretSnapshot(t *testing.T) map[string]vault.Record {
	t.Helper()
	var records map[string]vault.Record
	if err := vault.View(func(tx *vault.Tx) (err error) {
		records, err = tx.Records()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

func runFoldSecretImport(t *testing.T, args []string) {
	t.Helper()
	code, out, errs := captureOutput(t, func() int { return importCfg(args) })
	if strings.Contains(out+errs, "test-only-") {
		t.Fatal("fold exposed a credential")
	}
	if code != 0 {
		t.Fatalf("fold exit code = %d", code)
	}
}

func assertFoldSecretRow(t *testing.T, old string, present bool) {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if (reg.Providers[old] != nil) != present || reg.Providers["dev"] == nil {
		t.Fatalf("legacy row present = %t, want %t; destination present = %t", reg.Providers[old] != nil, present, reg.Providers["dev"] != nil)
	}
}

func assertFoldSecretMigration(t *testing.T, old string, before map[string]vault.Record) {
	t.Helper()
	after := foldSecretSnapshot(t)
	for _, method := range []string{providers.AuthOAuth, providers.AuthM2M, providers.AuthPAT} {
		src, dst := secretName(old, method), secretName("dev", method)
		value, found, err := vault.ReadSecret(dst)
		if err != nil || !found || value != "test-only-legacy-"+method {
			t.Errorf("%s destination credential missing or undecryptable", method)
		}
		if _, found := after[src]; found {
			t.Errorf("%s legacy sealed record remains", method)
		}
		from, to := before[src], after[dst]
		from.Sealed, to.Sealed = nil, nil
		if !reflect.DeepEqual(from, to) {
			t.Errorf("%s record metadata changed", method)
		}
	}
}

// TestSealImportedEmptySecretIsNotSealed pins that a row with no readable secret fails and is not reported sealed.
func TestSealImportedEmptySecretIsNotSealed(t *testing.T) {
	useVaultTest(t)
	cfg := writeCfg(t, "[pat-empty]\nhost = https://pat.example.cloud.databricks.com\ntoken = \n")
	row := Row{ID: "pat-empty", Name: "pat-empty", Auth: providers.AuthPAT, HasSecret: true, Action: "add"}
	name := SecretNameOf(row)
	if name == "" {
		t.Fatal("row has no secret name")
	}
	code, stdout, stderr := captureOutput(t, func() int { return sealImported(cfg, []Row{row}) })
	if code != 1 || strings.Contains(stdout, "sealed") || !strings.Contains(stderr, "no secret in the source section") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, src, _ := vault.Lookup(name); src == "vault" {
		t.Errorf("%s stored in the vault", name)
	}
}
