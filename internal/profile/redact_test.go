package profile

import (
	"bytes"
	"strings"
	"testing"
)

const sentinel = "SENTINEL123"

func TestMigrateDryRunDiffMasksHeaders(t *testing.T) {
	doc := strings.Replace(twinPair, `"models":{"opus":"databricks-gpt-5-5"}`,
		`"headers":{"Authorization":"Bearer `+sentinel+`"},"models":{"opus":"databricks-gpt-5-5"}`, 1)
	seed(t, `{"providers":{`+doc+`}}`)
	oldDoc, newDoc, _, err := Migrate(false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if !PrintDiff(&out, oldDoc, newDoc) {
		t.Fatal("migrate produced no diff")
	}
	if strings.Contains(out.String(), sentinel) {
		t.Fatalf("diff leaks header value:\n%s", out.String())
	}
	removed := false
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "- ") && strings.Contains(l, "Authorization") && strings.Contains(l, "<redacted>") {
			removed = true
		}
	}
	if !removed {
		t.Errorf("want a removed line showing Authorization as <redacted>:\n%s", out.String())
	}
}

func TestPrintDiffMasksHeadersAndSecretKeysNearChange(t *testing.T) {
	mk := func(base string) []byte {
		return []byte(`{"providers":{"gw":{"base_url":"` + base + `","headers":{"Authorization":"Bearer ` + sentinel +
			`"},"api_key":"` + sentinel + `","client_secret":"` + sentinel + `","kind":"anthropic"}}}`)
	}
	var out bytes.Buffer
	if !PrintDiff(&out, mk("https://a"), mk("https://b")) {
		t.Fatal("no diff")
	}
	got := out.String()
	if strings.Contains(got, sentinel) {
		t.Fatalf("context lines leak a secret:\n%s", got)
	}
	if !strings.Contains(got, "https://a") || !strings.Contains(got, "https://b") || !strings.Contains(got, "<redacted>") {
		t.Errorf("non-secret change must still show, secrets as <redacted>:\n%s", got)
	}
}

func TestPrintDiffMasksChangedHeaderValue(t *testing.T) {
	mk := func(v string) []byte { return []byte(`{"providers":{"gw":{"headers":{"X-Token":"` + v + `"}}}}`) }
	var out bytes.Buffer
	PrintDiff(&out, mk(sentinel+"a"), mk(sentinel+"b"))
	if strings.Contains(out.String(), sentinel) {
		t.Fatalf("changed header value leaks:\n%s", out.String())
	}
}

func TestUseDryRunDiffMasksHeadersInContext(t *testing.T) {
	seed(t, `{"providers":{"gw":{"kind":"anthropic","base_url":"https://gw/a","auth":{"type":"none"},"headers":{"Authorization":"Bearer `+sentinel+
		`"},"models":{"opus":"claude-opus-4-6"}}},"profiles":{"sets":{"w":{"session":{"account":"gw"}}}}}`)
	oldDoc, newDoc, err := Use(false, "w")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if !PrintDiff(&out, oldDoc, newDoc) {
		t.Fatal("use produced no diff")
	}
	if strings.Contains(out.String(), sentinel) {
		t.Fatalf("use --dry-run diff leaks:\n%s", out.String())
	}
}
