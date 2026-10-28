package vaultmigrate

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const dbxRow = `{"auth":{"authType":"u2m"},"cliProfile":"%s","host":"https://dbc-1.cloud.databricks.com","preset":"databricks"}`

const copilotRow = `{"kind":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli"}}`

func row(name string) string { return strings.Replace(dbxRow, "%s", name, 1) }

// seed writes a schema-2 vault holding the given overlay/providers doc and pins; no secrets.
func seed(t *testing.T, overlay string, pins map[string]string) {
	t.Helper()
	vaulttest.Use(t)
	if err := vault.Update(func(tx *vault.Tx) error {
		for n, v := range pins {
			if err := tx.Put(vault.Pins, n, v); err != nil {
				return err
			}
		}
		if overlay != "" {
			if err := tx.Put(vault.Overlay, "providers", overlay); err != nil {
				return err
			}
		}
		return tx.SetSchema(2)
	}); err != nil {
		t.Fatal(err)
	}
}

func apply(t *testing.T) *Plan {
	t.Helper()
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	res, err := Apply(reg)
	if err != nil {
		t.Fatal(err)
	}
	return res.Plan
}

func plan(t *testing.T) *Plan {
	t.Helper()
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compute(reg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pin(t *testing.T, k string) string {
	t.Helper()
	var v string
	_ = vault.View(func(tx *vault.Tx) error { v, _ = tx.Get(vault.Pins, k); return nil })
	return v
}

func overlayDoc(t *testing.T) string {
	t.Helper()
	d, err := providers.OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	return string(d)
}

func hasLine(p *Plan, old string) bool {
	for _, c := range p.Changes {
		if c.Old == old {
			return true
		}
	}
	return false
}

func TestRetiredCopilotRowBecomesPersonalAndStampsLogin(t *testing.T) {
	seed(t, `{"providers":{"github-copilot":`+copilotRow+`,"dev":`+row("dev")+`}}`,
		map[string]string{"COPILOT_PROFILE": "edotau"})
	apply(t)
	reg, _ := Current()
	if reg.Providers["github-copilot"] != nil {
		t.Error("retired github-copilot row survived")
	}
	if p := reg.Providers["personal"]; p == nil || p.Provider != "github" || p.Login != "edotau" {
		t.Errorf("personal = %+v", p)
	}
	// negative control: an unrelated profile name is untouched
	if reg.Providers["dev"] == nil || reg.Providers["dev"].Provider != "databricks" {
		t.Error("unrelated profile dev changed")
	}
	if got := pin(t, "COPILOT_PROFILE"); got != "personal" {
		t.Errorf("COPILOT_PROFILE = %q, want personal", got)
	}
}

func TestPinLoginRewriteIsIdempotent(t *testing.T) {
	seed(t, `{"providers":{"github-copilot":`+copilotRow+`}}`, map[string]string{"COPILOT_GITHUB_USER": "edotau", "CLAUDE_PROFILE": "dev"})
	p := plan(t)
	found := false
	for _, c := range p.Changes {
		if c.Kind == KindPinKey && c.Old == "COPILOT_GITHUB_USER" && c.New == "COPILOT_PROFILE" && strings.Contains(c.Detail, "edotau -> personal") {
			found = true
		}
	}
	if !found {
		t.Errorf("plan lacks the login pin rewrite: %+v", p.Changes)
	}
	apply(t)
	if got := pin(t, "COPILOT_PROFILE"); got != "personal" {
		t.Fatalf("COPILOT_PROFILE = %q", got)
	}
	if got := pin(t, "CLAUDE_PROFILE"); got != "dev" {
		t.Errorf("unrelated pin CLAUDE_PROFILE = %q", got)
	}
	if p2 := plan(t); !p2.Empty() {
		t.Errorf("second plan not empty: %+v", p2.Changes)
	}
	if p3 := apply(t); !p3.Empty() || pin(t, "COPILOT_PROFILE") != "personal" {
		t.Errorf("rerun changed things: %+v", p3.Changes)
	}
}

// Observed behavior, not endorsed: a login pin that differs from personal's recorded login is left as the raw login.
func TestPinLoginDiffersFromPersonalLoginIsLeftAlone(t *testing.T) {
	seed(t, `{"providers":{"personal":{"kind":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli"},"login":"someone-else"}}}`,
		map[string]string{"COPILOT_PROFILE": "edotau"})
	apply(t)
	if got := pin(t, "COPILOT_PROFILE"); got != "edotau" {
		t.Errorf("pin = %q; expected the unmatched login to stay as-is", got)
	}
	reg, _ := Current()
	if l := reg.Providers["personal"].Login; l != "someone-else" {
		t.Errorf("personal login overwritten: %q", l)
	}
}

func TestPinNamingExistingProfileIsUntouched(t *testing.T) {
	seed(t, "", map[string]string{"COPILOT_PROFILE": "personal"})
	if p := plan(t); hasLine(p, "COPILOT_PROFILE=personal") || hasLine(p, "COPILOT_PROFILE") {
		t.Errorf("plan touches a pin that already names a profile: %+v", p.Changes)
	}
}

func TestProfileSetsDroppedInPlanAndApplyIdempotent(t *testing.T) {
	withSets := `{"profiles":{"active":"x","sets":{"x":{"codex":"dev"}}},"providers":{"dev":` + row("dev") + `}}`
	seed(t, withSets, nil)
	if p := plan(t); !hasLine(p, "overlay/providers.profiles") {
		t.Fatalf("plan lacks the sets-drop line: %+v", p.Changes)
	}
	apply(t)
	if doc := overlayDoc(t); strings.Contains(doc, `"profiles"`) || strings.Contains(doc, `"sets"`) {
		t.Errorf("sets survived:\n%s", doc)
	}
	var stored string
	_ = vault.View(func(tx *vault.Tx) error { stored, _ = tx.Get(vault.Overlay, "providers"); return nil })
	if strings.Contains(stored, `"sets"`) {
		t.Errorf("stored doc keeps sets: %s", stored)
	}
	if p := plan(t); !p.Empty() {
		t.Errorf("second plan not empty: %+v", p.Changes)
	}
	before := overlayDoc(t)
	apply(t)
	if after := overlayDoc(t); after != before {
		t.Errorf("rerun changed the overlay:\n%s\n---\n%s", before, after)
	}
}

func TestOverlayWithoutSetsHasNoDropLineAndStaysByteIdentical(t *testing.T) {
	seed(t, `{"providers":{"dev":`+row("dev")+`}}`, nil)
	if p := plan(t); hasLine(p, "overlay/providers.profiles") {
		t.Errorf("drop line for an overlay with no sets: %+v", p.Changes)
	}
	apply(t) // nests it
	var first string
	_ = vault.View(func(tx *vault.Tx) error { first, _ = tx.Get(vault.Overlay, "providers"); return nil })
	apply(t)
	var second string
	_ = vault.View(func(tx *vault.Tx) error { second, _ = tx.Get(vault.Overlay, "providers"); return nil })
	if first != second {
		t.Errorf("stored overlay not byte-identical:\n%s\n---\n%s", first, second)
	}
}

func TestNestedOutputKeepsDeclaredProfileOrder(t *testing.T) {
	seed(t, `{"providers":{"prod":`+row("prod")+`,"dev":`+row("dev")+`,"qa":`+row("qa")+`}}`, nil)
	apply(t)
	doc := overlayDoc(t)
	ip, id, iq := strings.Index(doc, `"prod"`), strings.Index(doc, `"dev"`), strings.Index(doc, `"qa"`)
	if ip < 0 || id < 0 || iq < 0 || !(ip < id && id < iq) {
		t.Errorf("order prod,dev,qa lost (prod=%d dev=%d qa=%d):\n%s", ip, id, iq, doc)
	}
	before := doc
	apply(t)
	if overlayDoc(t) != before {
		t.Error("order changed on rerun")
	}
}

// KNOWN BUG (reported, product code untouched): with no overlay row for the profile, stampLogin writes a bare
// {"login":...} personal row, which replaces the built-in by name and makes providers.Load fail.
func TestStampLoginWithoutOverlayRowKeepsRegistryLoadable(t *testing.T) {
	seed(t, "", map[string]string{"COPILOT_GITHUB_USER": "edotau"})
	apply(t)
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Providers["personal"].Login != "edotau" {
		t.Errorf("login = %q", reg.Providers["personal"].Login)
	}
}
