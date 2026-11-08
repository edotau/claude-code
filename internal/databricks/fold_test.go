package databricks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

const (
	foldHashCache = "DATABRICKS_M2M_0123456789ABCDEF_ACCESS"
	foldDevHost   = "https://dev.example.cloud.databricks.com"
)

func foldProfile(host, sp string) map[string]any {
	return map[string]any{"preset": "databricks", "host": host, "auth": map[string]any{"type": "oauth", "spClientId": sp}}
}

func foldSource(host, client string) map[string]any {
	return map[string]any{"preset": "databricks", "host": host, "auth": map[string]any{"type": "m2m", "clientId": client}}
}

func setRow(t *testing.T, id string, fields map[string]any) {
	t.Helper()
	if err := providers.SetProviderFields(id, fields); err != nil {
		t.Fatal(err)
	}
}

// seedLegacyRows seeds profile <tag> and the name-unrelated m2m row acme-sp-<tag> for dev, qa and prod.
func seedLegacyRows(t *testing.T) {
	t.Helper()
	for _, tag := range []string{"dev", "qa", "prod"} {
		host := "https://" + tag + ".example.cloud.databricks.com"
		setRow(t, tag, foldProfile(host, "sp-"+tag))
		setRow(t, "acme-sp-"+tag, foldSource(host, "sp-"+tag))
	}
}

const foldCfg = "[dev]\nhost = " + foldDevHost + "\n"

func foldImport(t *testing.T, flags ...string) (int, string, string) {
	t.Helper()
	cfg := writeCfg(t, foldCfg)
	return captureOutput(t, func() int { return importCfg(append([]string{"--dbxcfg", cfg}, flags...)) })
}

func mustFold(t *testing.T, flags ...string) string {
	t.Helper()
	code, out, errs := foldImport(t, flags...)
	if code != 0 || strings.Contains(out+errs, "test-only-") {
		t.Fatalf("%v: code %d %s %s", flags, code, out, errs)
	}
	return out
}

func rowPresent(t *testing.T, id string) bool {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Providers[id] != nil
}

func TestImportFoldIsExplicitAndDryRun(t *testing.T) {
	useVaultTest(t)
	seedLegacyRows(t)
	for _, flags := range [][]string{{"--apply"}, {"--fold-legacy", "--dry-run", "--apply"}} {
		out := mustFold(t, flags...)
		if !rowPresent(t, "acme-sp-dev") {
			t.Fatalf("%v removed rows", flags)
		}
		if slices.Contains(flags, "--fold-legacy") && !strings.Contains(out, "fold acme-sp-dev -> dev") {
			t.Fatalf("missing fold preview: %s", out)
		}
	}
	mustFold(t, "--fold-legacy", "--apply")
	for _, tag := range []string{"dev", "qa", "prod"} {
		if rowPresent(t, "acme-sp-"+tag) || !rowPresent(t, tag) {
			t.Fatalf("fold %s failed", tag)
		}
	}
	before := foldSecretSnapshot(t)
	if out := mustFold(t, "--fold-legacy", "--apply"); strings.Contains(out, "fold ") {
		t.Fatalf("second fold not idempotent: %s", out)
	}
	if !reflect.DeepEqual(before, foldSecretSnapshot(t)) {
		t.Fatal("second fold changed sealed records")
	}
}

func TestFoldMatchesHostAndClientID(t *testing.T) {
	other := "https://other.example.cloud.databricks.com"
	for _, c := range []struct {
		name, want string
		src        map[string]any
		extra      bool
		folded     bool
	}{
		{"match", "fold acme-sp-dev -> dev", foldSource(foldDevHost, "sp-dev"), false, true},
		{"other host", "no profile pairs its host and client id", foldSource(other, "sp-dev"), false, false},
		{"other client", "no profile pairs its host and client id", foldSource(foldDevHost, "sp-x"), false, false},
		{"two profiles", "several profiles pair it: dev, dev2", foldSource(foldDevHost, "sp-dev"), true, false},
		{"not m2m", "", foldProfile(foldDevHost, ""), false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			useVaultTest(t)
			setRow(t, "dev", foldProfile(foldDevHost, "sp-dev"))
			if c.extra {
				setRow(t, "dev2", foldProfile(foldDevHost, "sp-dev"))
			}
			setRow(t, "acme-sp-dev", c.src)
			out := mustFold(t, "--fold-legacy", "--apply")
			if rowPresent(t, "acme-sp-dev") == c.folded {
				t.Fatalf("folded = %t, want %t: %s", !rowPresent(t, "acme-sp-dev"), c.folded, out)
			}
			if c.want == "" && (strings.Contains(out, "keep ") || strings.Contains(out, "fold ")) {
				t.Fatalf("unexpected fold output: %s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("output lacks %q: %s", c.want, out)
			}
		})
	}
}

// seedFoldReference makes acme-sp-dev referenced in the way kind names.
func seedFoldReference(t *testing.T, dir, kind string) {
	t.Helper()
	const old = "acme-sp-dev"
	if kind == "settings" {
		if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), []byte(`{"x":"`+old+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	err := vault.Update(func(tx *vault.Tx) error {
		switch kind {
		case "agent":
			return tx.Put(vault.Overlay, vault.AgentRecordsKey, `{"claude":{"profile":"`+old+`"}}`)
		case "alias":
			return tx.Put(vault.Overlay, "aliases", `{"dev":"`+old+`"}`)
		case "pin":
			return tx.Put(vault.Pins, "PROFILE", old)
		}
		return tx.Put(vault.Overlay, "other", `{"profile":"`+old+`"}`)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImportFoldReferenceVeto(t *testing.T) {
	for _, c := range []struct {
		kind   string
		folded bool
	}{{"agent", true}, {"pin", true}, {"alias", false}, {"overlay", false}, {"settings", false}} {
		t.Run(c.kind, func(t *testing.T) {
			dir := useVaultTest(t)
			seedLegacyRows(t)
			seedFoldReference(t, dir, c.kind)
			out := mustFold(t, "--fold-legacy", "--apply")
			if rowPresent(t, "acme-sp-dev") == c.folded {
				t.Fatalf("folded = %t, want %t: %s", c.folded, !c.folded, out)
			}
			if rowPresent(t, "acme-sp-qa") {
				t.Fatal("unrelated unreferenced row was not folded")
			}
			assertFoldRepoint(t, c.kind, c.folded, out)
		})
	}
}

func assertFoldRepoint(t *testing.T, kind string, folded bool, out string) {
	t.Helper()
	if !folded && kind != "settings" && !strings.Contains(out, "referenced by vault overlay") {
		t.Fatalf("missing veto reason: %s", out)
	}
	if kind == "agent" && providers.AgentRecord("claude").Profile != "dev" {
		t.Fatalf("agent pin = %q, want dev", providers.AgentRecord("claude").Profile)
	}
	if kind == "pin" {
		var v string
		_ = vault.View(func(tx *vault.Tx) error { v, _ = tx.Get(vault.Pins, "PROFILE"); return nil })
		if v != "dev" {
			t.Fatalf("PROFILE pin = %q, want dev", v)
		}
	}
}

// seedFoldSecrets seals the source M2M secret plus both cache kinds; dst, when set, seals dev's M2M value.
func seedFoldSecrets(t *testing.T, oauthHeld bool, dst string) {
	t.Helper()
	src := secretName("acme-sp-dev", providers.AuthM2M)
	names := map[string]string{src: "test-only-src", src + "_ACCESS": "test-only-cache", foldHashCache: "test-only-hash"}
	if dst != "" {
		names[secretName("dev", providers.AuthM2M)] = dst
	}
	if oauthHeld {
		names[secretName("acme-sp-dev", providers.AuthOAuth)] = "test-only-oauth"
	}
	for n, v := range names {
		if err := vault.Store(n, v); err != nil {
			t.Fatal(err)
		}
	}
}

func foldSecretNames(t *testing.T) []string {
	t.Helper()
	var names []string
	if err := vault.View(func(tx *vault.Tx) error { names = tx.Names(vault.Secrets); return nil }); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestImportFoldSealedCredentials(t *testing.T) {
	src, dst := secretName("acme-sp-dev", providers.AuthM2M), secretName("dev", providers.AuthM2M)
	for _, c := range []struct {
		name, dst  string
		oauthHeld  bool
		wantFolded bool
	}{{"none", "", false, true}, {"equal", "test-only-src", false, true}, {"differ", "test-only-other", false, false},
		{"oauth-held", "", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			useVaultTest(t)
			seedLegacyRows(t)
			seedFoldSecrets(t, c.oauthHeld, c.dst)
			before := foldSecretSnapshot(t)
			validKey := os.Getenv("HARNESS_VAULT_KEY")
			t.Setenv("HARNESS_VAULT_KEY", "invalid-key-must-not-be-read")
			mustFold(t, "--fold-legacy", "--apply", "--dry-run")
			t.Setenv("HARNESS_VAULT_KEY", validKey)
			if !reflect.DeepEqual(before, foldSecretSnapshot(t)) || !rowPresent(t, "acme-sp-dev") {
				t.Fatal("preview changed state")
			}
			code, out, errs := foldImport(t, "--fold-legacy", "--apply")
			assertFoldSecretOutcome(t, c.name, c.wantFolded, code, out+errs)
			after := foldSecretNames(t)
			if c.wantFolded {
				assertFoldSecretMoved(t, before, src, dst, c.dst == "")
			} else if got := foldSecretSnapshot(t); !reflect.DeepEqual(before[src], got[src]) || !reflect.DeepEqual(before[dst], got[dst]) {
				t.Fatal("kept row changed its sealed records")
			}
			if c.wantFolded && (slices.Contains(after, src+"_ACCESS") || slices.Contains(after, foldHashCache)) {
				t.Fatalf("caches remain: %v", after)
			}
		})
	}
	t.Run("no credentials needs no key", func(t *testing.T) {
		useVaultTest(t)
		seedLegacyRows(t)
		t.Setenv("HARNESS_VAULT_KEY", "invalid-key-must-not-be-read")
		mustFold(t, "--fold-legacy", "--apply")
		if rowPresent(t, "acme-sp-dev") {
			t.Fatal("row without credentials was not folded")
		}
	})
}

func assertFoldSecretOutcome(t *testing.T, name string, folded bool, code int, out string) {
	t.Helper()
	if strings.Contains(out, "test-only-") {
		t.Fatal("fold exposed a credential")
	}
	wantCode := 0
	if name == "differ" {
		wantCode = 1
	}
	if code != wantCode || rowPresent(t, "acme-sp-dev") == folded {
		t.Fatalf("code %d folded=%t: %s", code, !rowPresent(t, "acme-sp-dev"), out)
	}
	if name == "differ" && !strings.Contains(out, "differs from") {
		t.Fatalf("differ error missing: %s", out)
	}
}

// assertFoldSecretMoved checks the M2M source record is gone and dst opens to the source value.
func assertFoldSecretMoved(t *testing.T, before map[string]vault.Record, src, dst string, moved bool) {
	t.Helper()
	after := foldSecretSnapshot(t)
	if _, found := after[src]; found {
		t.Fatal("source record remains")
	}
	if v, found, err := vault.ReadSecret(dst); err != nil || !found || v != "test-only-src" {
		t.Fatalf("destination unreadable: %v %v", found, err)
	}
	if moved {
		from, to := before[src], after[dst]
		from.Sealed, to.Sealed = nil, nil
		if !reflect.DeepEqual(from, to) {
			t.Fatal("record metadata changed")
		}
	} else if !reflect.DeepEqual(before[dst], after[dst]) {
		t.Fatal("equal destination record changed")
	}
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

// planFor runs planFold against the seeded overlay without writing.
func planFor(t *testing.T) foldPlan {
	t.Helper()
	var plan foldPlan
	_, _, err := providers.UpdateOverlayDoc(false, func(top map[string]json.RawMessage, tx *vault.Tx) (err error) {
		plan, err = planFold(top, tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestFoldVetoesEnvValueEmbeddingID(t *testing.T) {
	useVaultTest(t)
	seedLegacyRows(t)
	if err := vault.Update(func(tx *vault.Tx) error {
		return tx.Put(vault.Env, "LAUNCH", "databricks auth --profile acme-sp-dev")
	}); err != nil {
		t.Fatal(err)
	}
	plan := planFor(t)
	if !slices.ContainsFunc(plan.Kept, func(k string) bool { return strings.HasPrefix(k, "acme-sp-dev: referenced by vault env LAUNCH") }) {
		t.Fatalf("env value embedding the id did not veto: %v", plan.Kept)
	}
}

func TestFoldPlanFlagsSharedDestinationAsDup(t *testing.T) {
	useVaultTest(t)
	setRow(t, "dev", foldProfile(foldDevHost, "sp-dev"))
	setRow(t, "a-sp", foldSource(foldDevHost, "sp-dev"))
	setRow(t, "b-sp", foldSource(foldDevHost, "sp-dev"))
	for _, id := range []string{"a-sp", "b-sp"} {
		if err := vault.Store(secretName(id, providers.AuthM2M), "test-only-"+id); err != nil {
			t.Fatal(err)
		}
	}
	plan := planFor(t)
	if len(plan.Moves) != 2 || plan.Moves[0].Dup || !plan.Moves[1].Dup {
		t.Fatalf("want first move, second dup: %+v", plan.Moves)
	}
}

func TestFoldPlanKeepsHashCachesWithoutMoves(t *testing.T) {
	useVaultTest(t)
	setRow(t, "orphan", foldSource(foldDevHost, "sp-none"))
	if err := vault.Store(foldHashCache, "test-only-hash"); err != nil {
		t.Fatal(err)
	}
	if plan := planFor(t); !plan.empty() {
		t.Fatalf("hash cache alone queued for deletion: %+v", plan)
	}
}

func TestFoldKeepsRowWithNonVaultCredential(t *testing.T) {
	useVaultTest(t)
	setRow(t, "dev", foldProfile(foldDevHost, "sp-dev"))
	src := foldSource(foldDevHost, "sp-dev")
	src["auth"].(map[string]any)["command"] = "get-secret"
	setRow(t, "acme-sp-dev", src)
	plan := planFor(t)
	if len(plan.Moves) != 0 || !slices.ContainsFunc(plan.Kept, func(k string) bool { return strings.HasPrefix(k, "acme-sp-dev: takes its credential") }) {
		t.Fatalf("command-credential row not kept: %+v", plan)
	}
}

func TestFoldLeavesAgentRecordsAloneWhenNoneNameRow(t *testing.T) {
	useVaultTest(t)
	setRow(t, "dev", foldProfile(foldDevHost, "sp-dev"))
	setRow(t, "acme-sp-dev", foldSource(foldDevHost, "sp-dev"))
	const raw = `{"claude": {"profile": "other"}}`
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, vault.AgentRecordsKey, raw) }); err != nil {
		t.Fatal(err)
	}
	mustFold(t, "--fold-legacy", "--apply")
	var got string
	_ = vault.View(func(tx *vault.Tx) error { got, _ = tx.Get(vault.Overlay, vault.AgentRecordsKey); return nil })
	if rowPresent(t, "acme-sp-dev") || got != raw {
		t.Fatalf("agent-records rewritten without a matching pin: %q", got)
	}
}

func TestFoldEnvVetoMatchesWholeIDOnly(t *testing.T) {
	useVaultTest(t)
	setRow(t, "prof", foldProfile(foldDevHost, "sp-dev"))
	setRow(t, "dev", foldSource(foldDevHost, "sp-dev"))
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Env, "MODE", "development") }); err != nil {
		t.Fatal(err)
	}
	plan := planFor(t)
	if len(plan.Moves) != 1 || len(plan.Kept) != 0 {
		t.Fatalf("env value 'development' vetoed row 'dev': %+v", plan)
	}
}
