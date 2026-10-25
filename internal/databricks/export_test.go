package databricks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

const exportSeedCfg = `[DEFAULT]
host = https://default.example.cloud.databricks.com
token = dapi-DEFAULT-SENTINEL

[alice]
host = https://alice.example.cloud.databricks.com
auth_type = databricks-cli

[svc]
host = https://svc.example.cloud.databricks.com
client_id = cid-svc
client_secret = CLIENT-SECRET-SENTINEL

[tok]
host = https://tok.example.cloud.databricks.com
token = PAT-SENTINEL
`

var sentinels = []string{"CLIENT-SECRET-SENTINEL", "PAT-SENTINEL", "dapi-DEFAULT-SENTINEL", "REFRESH-SENTINEL"}

// seedExportVault imports exportSeedCfg with sealed secrets plus a sealed refresh token for alice.
func seedExportVault(t *testing.T) {
	t.Helper()
	useVaultTest(t)
	cfg := filepath.Join(t.TempDir(), "src.cfg")
	if err := os.WriteFile(cfg, []byte(exportSeedCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := captureOutput(t, func() int {
		return importCfg([]string{"--file", cfg, "--apply", "--seal-secrets"})
	}); code != 0 {
		t.Fatalf("seed import: %d %s %s", code, out, errs)
	}
	if err := providers.SealSecret("PROVIDER_ALICE_REFRESH_TOKEN", "REFRESH-SENTINEL"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"PROVIDER_SVC_CLIENT_SECRET", "PROVIDER_TOK_PAT"} {
		if v, _, err := secrets.Lookup(n); err != nil || v == "" {
			t.Fatalf("seed: %s not sealed (%v)", n, err)
		}
	}
}

func noSentinel(t *testing.T, what, s string) {
	t.Helper()
	for _, v := range sentinels {
		if strings.Contains(s, v) {
			t.Errorf("%s contains secret value %q", what, v)
		}
	}
}

func TestExportNeverContainsASecretValue(t *testing.T) {
	seedExportVault(t)
	code, out, errs := captureOutput(t, func() int { return export(nil) })
	if code != 0 || !strings.Contains(out, "[svc]") {
		t.Fatalf("export: %d %q %q", code, out, errs)
	}
	noSentinel(t, "stdout", out)
	noSentinel(t, "stderr", errs)
	if strings.Contains(out, "client_secret") || strings.Contains(out, "token =") {
		t.Errorf("secret-bearing key written:\n%s", out)
	}
	file := filepath.Join(t.TempDir(), "out.cfg")
	if code, _, errs := captureOutput(t, func() int { return export([]string{"--out", file}) }); code != 0 {
		t.Fatalf("export --out: %d %s", code, errs)
	} else {
		noSentinel(t, "--out stderr", errs)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	noSentinel(t, "--out file", string(b))
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("--out mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestExportMergeNeverWritesDefaultOrOverwritesU2M(t *testing.T) {
	seedExportVault(t)
	const existing = "[DEFAULT]\nhost = https://mine.example.cloud.databricks.com\ntoken = KEEP-DEFAULT\n\n" +
		"[alice]\nhost = https://login.example.cloud.databricks.com\nauth_type = databricks-cli\n"
	file := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(file, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--merge", "--out", file}, {"--merge", "--force", "--out", file}} {
		if code, _, errs := captureOutput(t, func() int { return export(args) }); code != 0 {
			t.Fatalf("%v: %d %s", args, code, errs)
		}
	}
	b, _ := os.ReadFile(file)
	got := string(b)
	if !strings.HasPrefix(got, "[DEFAULT]\nhost = https://mine.example.cloud.databricks.com\ntoken = KEEP-DEFAULT\n") {
		t.Errorf("[DEFAULT] changed:\n%s", got)
	}
	if strings.Count(got, "[DEFAULT]") != 1 || !strings.Contains(got, "https://login.example.cloud.databricks.com") ||
		strings.Contains(got, "https://alice.example.cloud.databricks.com") {
		t.Errorf("databricks-cli section overwritten or DEFAULT duplicated:\n%s", got)
	}
	if !strings.Contains(got, "[svc]") || !strings.Contains(got, "[tok]") {
		t.Errorf("new sections not merged:\n%s", got)
	}
	noSentinel(t, "merged file", got)
}

func TestExportRefusesToOverwriteWithoutForce(t *testing.T) {
	seedExportVault(t)
	file := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(file, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := captureOutput(t, func() int { return export([]string{"--out", file}) }); code == 0 {
		t.Error("overwrote an existing --out without --force")
	}
	if b, _ := os.ReadFile(file); string(b) != "keep\n" {
		t.Errorf("file changed: %q", b)
	}
}

func rowsJSON(t *testing.T) map[string]string {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, id := range []string{"alice", "svc", "tok"} {
		p := reg.Providers[id]
		if p == nil {
			t.Fatalf("row %s missing", id)
		}
		b, _ := json.Marshal(p)
		out[id] = string(b)
	}
	return out
}

func TestExportImportRoundTripYieldsIdenticalRows(t *testing.T) {
	seedExportVault(t)
	before := rowsJSON(t)
	file := filepath.Join(t.TempDir(), "rt.cfg")
	if code, _, errs := captureOutput(t, func() int { return export([]string{"--out", file}) }); code != 0 {
		t.Fatalf("export: %d %s", code, errs)
	}
	useVaultTest(t) // a fresh vault: the rows must come back from the file alone
	if code, out, errs := captureOutput(t, func() int { return importCfg([]string{"--file", file, "--apply"}) }); code != 0 {
		t.Fatalf("import: %d %s %s", code, out, errs)
	}
	after := rowsJSON(t)
	for id, want := range before {
		if after[id] != want {
			t.Errorf("row %s differs after round trip\nwant %s\ngot  %s", id, want, after[id])
		}
	}
}
