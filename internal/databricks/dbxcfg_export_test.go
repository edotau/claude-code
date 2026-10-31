package databricks

import (
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func sampleRows() []ExportRow {
	return []ExportRow{
		{ID: "dev", Section: "dev", Host: "https://dev.example.cloud.databricks.com", Auth: providers.AuthOAuth},
		{ID: "svc", Section: "svc", Host: "https://svc.example.cloud.databricks.com", Auth: providers.AuthM2M, ClientID: "cid-svc"},
		{ID: "tok", Section: "tok", Host: "https://tok.example.cloud.databricks.com", Auth: providers.AuthPAT},
	}
}

func TestRenderCarriesNoSecretKeys(t *testing.T) {
	out := RenderExport(sampleRows())
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
	rows, skipped := Rename(sampleRows(), map[string]string{"dev": "default", "svc": "__settings__", "tok": "bad name"})
	if len(rows) != 0 || len(skipped) != 3 {
		t.Errorf("rows %v skipped %v", rows, skipped)
	}
}

func TestMergeNeverWritesDefault(t *testing.T) {
	const existing = "[DEFAULT]\nhost = https://d.example.cloud.databricks.com\ntoken = keep-me\n"
	for _, force := range []bool{false, true} {
		out, _ := Merge(existing, []ExportRow{{ID: "d", Section: "DEFAULT", Host: "https://x.example.cloud.databricks.com", Auth: providers.AuthPAT}}, force)
		if out != existing {
			t.Errorf("force=%v: [DEFAULT] touched:\n%s", force, out)
		}
	}
	out, _ := Merge("", []ExportRow{{ID: "d", Section: "default", Host: "https://x.example.cloud.databricks.com", Auth: providers.AuthPAT}}, true)
	if strings.Contains(strings.ToLower(out), "[default]") {
		t.Errorf("[default] created in an empty file:\n%s", out)
	}
}

func TestMergeNeverOverwritesU2MSection(t *testing.T) {
	const existing = "[dev]\nhost = https://mine.example.cloud.databricks.com\nauth_type = databricks-cli\n"
	out, notes := Merge(existing, sampleRows()[:1], true)
	if out != existing || !strings.Contains(strings.Join(notes, "\n"), "never overwritten") {
		t.Errorf("databricks-cli section overwritten (force):\n%s\n%v", out, notes)
	}
}

func TestMergeKeepsOthersAndReplacesOnlyWithForce(t *testing.T) {
	const existing = "[svc]\nhost = https://old.example.cloud.databricks.com\nclient_id = old\n\n[other]\nhost = https://o.example.cloud.databricks.com\n"
	svc := sampleRows()[1:2]
	if out, _ := Merge(existing, svc, false); out != existing {
		t.Errorf("replaced without force:\n%s", out)
	}
	out, _ := Merge(existing, svc, true)
	if !strings.Contains(out, "client_id = cid-svc") || strings.Contains(out, "client_id = old") || !strings.Contains(out, "[other]") {
		t.Errorf("force merge:\n%s", out)
	}
}

func TestExportThenCandidatesRoundTrip(t *testing.T) {
	m := candsOf(t, RenderExport(sampleRows()))
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
