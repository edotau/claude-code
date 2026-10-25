package launch

import (
	"bytes"
	"strings"
	"testing"
)

func TestProviderEnvNeverOverwritesPlanKeys(t *testing.T) {
	t.Setenv("GW_REGION", "")
	t.Setenv("ANTHROPIC_BASE_URL", "") // else providerEnv's process-env skip hides the guard under test
	plan := &Plan{Set: map[string]string{"ANTHROPIC_BASE_URL": "https://harness"}}
	applyOverlay(plan, true, nil, "", map[string]string{"ANTHROPIC_BASE_URL": "https://evil", "GW_REGION": "eu"})
	if plan.Set["ANTHROPIC_BASE_URL"] != "https://harness" {
		t.Errorf("provider env overwrote a harness key: %q", plan.Set["ANTHROPIC_BASE_URL"])
	}
	if plan.Set["GW_REGION"] != "eu" {
		t.Errorf("Set[GW_REGION] = %q, want eu", plan.Set["GW_REGION"])
	}
}

func TestPrintRedactsCredentialSegmentNames(t *testing.T) {
	set := map[string]string{"DATA_PATH": "/visible"}
	for _, k := range []string{"FOO_HEADERS", "MY_AUTH", "DB_CREDENTIALS", "DATABRICKS_PAT", "X_BEARER", "X_AUTH_Y"} {
		set[k] = "s3cr3t-value"
	}
	var b bytes.Buffer
	(&Plan{Set: set}).Print(&b)
	out := b.String()
	if strings.Contains(out, "s3cr3t-value") {
		t.Errorf("Print leaked a credential-shaped value:\n%s", out)
	}
	if !strings.Contains(out, "DATA_PATH=/visible") {
		t.Errorf("benign DATA_PATH was redacted:\n%s", out)
	}
}

func TestSessionSelectionKeepsRowAccountOnPrefixedModel(t *testing.T) {
	profileFixtureLaunch(t, "w", `{"account":"gw","models":{"sonnet":"m-son"}}`)
	_, sel, applied, err := sessionSelection(Options{Model: "plain:other"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider == nil || sel.Provider.Name != "gw" || applied != "w" {
		t.Fatalf("provider %v applied %q, want gw / w", sel.Provider, applied)
	}
	if got := sel.Slots["sonnet"].Model; got != "m-son" {
		t.Errorf("sonnet slot = %q, want m-son", got)
	}
}
