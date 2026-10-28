package vaultmigrate

import (
	"strings"
	"testing"
)

// A nested, already-current overlay whose only old part is a dead one_m key on qa.
const oneMOverlay = `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://dbc-2.cloud.databricks.com",` +
	`"auth":{"type":"oauth","header":"bearer"},"one_m":false}}}}`

func TestDeadOneMKeyIsPlannedDroppedAndIdempotent(t *testing.T) {
	seed(t, oneMOverlay, nil)
	p := plan(t)
	if !hasLine(p, "overlay/providers.qa.one_m") || hasLine(p, "overlay/providers") {
		t.Fatalf("plan = %+v, want only the one_m line", p.Changes)
	}
	apply(t)
	if doc := overlayDoc(t); strings.Contains(doc, "one_m") || !strings.Contains(doc, `"qa"`) {
		t.Fatalf("overlay after apply:\n%s", doc)
	}
	if again := plan(t); !again.Empty() {
		t.Fatalf("rerun plan = %+v, want nothing to migrate", again.Changes)
	}
}

func TestOldAuthSpellingConvertsToCurrentType(t *testing.T) {
	seed(t, `{"providers":{"qa":{"preset":"databricks","host":"https://dbc-2.cloud.databricks.com",`+
		`"auth":{"type":"bearer","authType":"oauth-m2m","clientId":"c"},"options":{"wire":"gateway"}}}}`, nil)
	apply(t)
	doc := overlayDoc(t)
	for _, want := range []string{`"type":"m2m"`, `"header":"bearer"`, `"wire":"ai-gateway"`} {
		if !strings.Contains(strings.ReplaceAll(doc, " ", ""), want) {
			t.Errorf("overlay lacks %s:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "authType") {
		t.Errorf("old authType survived:\n%s", doc)
	}
}
