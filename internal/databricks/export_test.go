package databricks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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
		return importCfg([]string{"--dbxcfg", cfg, "--apply", "--seal-secrets"})
	}); code != 0 {
		t.Fatalf("seed import: %d %s %s", code, out, errs)
	}
	if err := vault.Store("DATABRICKS_ALICE_OAUTH", `{"refresh":"REFRESH-SENTINEL"}`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"DATABRICKS_SVC_M2M", "DATABRICKS_TOK_PAT"} {
		if v, _, err := vault.Lookup(n); err != nil || v == "" {
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
	if code, out, errs := captureOutput(t, func() int { return importCfg([]string{"--dbxcfg", file, "--apply"}) }); code != 0 {
		t.Fatalf("import: %d %s %s", code, out, errs)
	}
	after := rowsJSON(t)
	for id, want := range before {
		if after[id] != want {
			t.Errorf("row %s differs after round trip\nwant %s\ngot  %s", id, want, after[id])
		}
	}
}

func TestExportStdoutWriteErrorExitsNonzero(t *testing.T) {
	seedExportVault(t)
	f, err := os.Open(os.DevNull) // read-only handle: every write fails
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = orig })
	if code, _, _ := captureOutput(t, func() int { os.Stdout = f; return export(nil) }); code == 0 {
		t.Error("export exited 0 although the stdout write failed")
	}
}

func sampleRows() []exportRow {
	return []exportRow{
		{ID: "dev", Section: "dev", Host: "https://dev.example.cloud.databricks.com", Auth: providers.AuthOAuth},
		{ID: "svc", Section: "svc", Host: "https://svc.example.cloud.databricks.com", Auth: providers.AuthM2M, ClientID: "cid-svc"},
		{ID: "tok", Section: "tok", Host: "https://tok.example.cloud.databricks.com", Auth: providers.AuthPAT},
	}
}

func TestRenderCarriesNoSecretKeys(t *testing.T) {
	out := renderExport(sampleRows())
	for _, bad := range []string{"client_secret", "token =", "dapi"} {
		if strings.Contains(out, bad) {
			t.Errorf("render contains %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "auth_type = databricks-cli") || !strings.Contains(out, "client_id = cid-svc") {
		t.Errorf("render lacks expected keys:\n%s", out)
	}
}

func TestRenameDropsReservedSections(t *testing.T) {
	rows, skipped := renameRows(sampleRows(), map[string]string{"dev": "default", "svc": "__settings__", "tok": "bad name"})
	if len(rows) != 0 || len(skipped) != 3 {
		t.Errorf("rows %v skipped %v", rows, skipped)
	}
}

func TestMergeNeverWritesDefault(t *testing.T) {
	const existing = "[DEFAULT]\nhost = https://d.example.cloud.databricks.com\ntoken = keep-me\n"
	for _, force := range []bool{false, true} {
		out, _ := mergeExport(existing, []exportRow{{ID: "d", Section: "DEFAULT", Host: "https://x.example.cloud.databricks.com", Auth: providers.AuthPAT}}, force)
		if out != existing {
			t.Errorf("force=%v: [DEFAULT] touched:\n%s", force, out)
		}
	}
	out, _ := mergeExport("", []exportRow{{ID: "d", Section: "default", Host: "https://x.example.cloud.databricks.com", Auth: providers.AuthPAT}}, true)
	if strings.Contains(strings.ToLower(out), "[default]") {
		t.Errorf("[default] created in an empty file:\n%s", out)
	}
}

func TestMergeNeverOverwritesU2MSection(t *testing.T) {
	const existing = "[dev]\nhost = https://mine.example.cloud.databricks.com\nauth_type = databricks-cli\n"
	out, notes := mergeExport(existing, sampleRows()[:1], true)
	if out != existing || !strings.Contains(strings.Join(notes, "\n"), "never overwritten") {
		t.Errorf("databricks-cli section overwritten (force):\n%s\n%v", out, notes)
	}
}

func TestMergeKeepsOthersAndReplacesOnlyWithForce(t *testing.T) {
	const existing = "[svc]\nhost = https://old.example.cloud.databricks.com\nclient_id = old\n\n[other]\nhost = https://o.example.cloud.databricks.com\n"
	svc := sampleRows()[1:2]
	if out, _ := mergeExport(existing, svc, false); out != existing {
		t.Errorf("replaced without force:\n%s", out)
	}
	out, _ := mergeExport(existing, svc, true)
	if !strings.Contains(out, "client_id = cid-svc") || strings.Contains(out, "client_id = old") || !strings.Contains(out, "[other]") {
		t.Errorf("force merge:\n%s", out)
	}
}

func TestExportThenCandidatesRoundTrip(t *testing.T) {
	m := candsOf(t, renderExport(sampleRows()))
	for _, r := range sampleRows() {
		c, ok := m[r.Section]
		if !ok || c.Auth != r.Auth || c.Host != r.Host || c.ClientID != r.ClientID || c.Skip != "" {
			t.Errorf("%s: exported %+v re-imported as %+v", r.ID, r, c)
		}
	}
}

// Characterizes maskPair across objects, arrays, headers and one-sided documents.
func TestMaskPair(t *testing.T) {
	o := map[string]any{"api_key": "a", "name": "x", "headers": map[string]any{"X": "h"}, "gone": "g", "list": []any{map[string]any{"token": "t"}}}
	n := map[string]any{"api_key": "b", "name": "y", "headers": map[string]any{"X": "h"}, "added": "z", "clientSecret": "s", "list": []any{map[string]any{"token": "t"}, "extra"}}
	go1, gn := maskPair(o, n, false, "")
	wantO := map[string]any{"api_key": redacted, "name": "x", "headers": map[string]any{"X": redacted}, "gone": "g",
		"list": []any{map[string]any{"token": redacted}}}
	wantN := map[string]any{"api_key": redactedChanged, "name": "y", "headers": map[string]any{"X": redacted}, "added": "z",
		"clientSecret": redacted, "list": []any{map[string]any{"token": redacted}, "extra"}}
	if !reflect.DeepEqual(go1, wantO) || !reflect.DeepEqual(gn, wantN) {
		t.Fatalf("got\n%v\n%v\nwant\n%v\n%v", go1, gn, wantO, wantN)
	}
	// One side is not an object: it comes back untouched while the object side is masked.
	ro, rn := maskPair("plain", map[string]any{"password": "p"}, false, "")
	if ro != "plain" || !reflect.DeepEqual(rn, map[string]any{"password": redacted}) {
		t.Fatalf("one-sided: %v %v", ro, rn)
	}
	// Leaf: a non-credential key stays; a credential key with equal values is not marked changed.
	if a, b := maskPair("v", "w", false, "name"); a != "v" || b != "w" {
		t.Fatalf("plain leaf: %v %v", a, b)
	}
	if a, b := maskPair("v", "v", false, "token"); a != redacted || b != redacted {
		t.Fatalf("equal secret: %v %v", a, b)
	}
}
