package vaultdoc

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// SetEnv and UnsetEnv write the env bucket the menu writes, report old and new, and honor dryRun.
func TestSetEnvUnsetEnvRoundTrip(t *testing.T) {
	vaulttest.Use(t)
	steps := []struct {
		unset, dry  bool
		value       string
		old, new    string
		bucketAfter string
	}{
		{value: " host-a ", old: "", new: "host-a", bucketAfter: "host-a"},
		{value: "host-b", dry: true, old: "host-a", new: "host-b", bucketAfter: "host-a"},
		{value: "host-b", old: "host-a", new: "host-b", bucketAfter: "host-b"},
		{unset: true, dry: true, old: "host-b", new: "", bucketAfter: "host-b"},
		{unset: true, old: "host-b", new: "", bucketAfter: ""},
	}
	for i, s := range steps {
		var ch EnvChange
		var err error
		if s.unset {
			ch, err = UnsetEnv("CLAUDE_REMOTE_HOST", s.dry)
		} else {
			ch, err = SetEnv("CLAUDE_REMOTE_HOST", s.value, s.dry)
		}
		if got := providers.CustomEnv()["CLAUDE_REMOTE_HOST"]; err != nil || ch.Old != s.old || ch.New != s.new || got != s.bucketAfter {
			t.Errorf("step %d: change %+v err %v bucket %q; want old %q new %q bucket %q", i, ch, err, got, s.old, s.new, s.bucketAfter)
		}
	}
}

// A secret never goes through the env bucket: owned names, bad names, empty values and @secret values are refused.
func TestSetEnvRefusesSecretsAndBadInput(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Store("MY_SEALED", "s3cret-value"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ name, value, want string }{
		"provider auth env": {"ANTHROPIC_API_KEY", "x", "is a secret name"},
		"harness name":      {"HARNESS_VAULT_KEY", "x", "is a secret name"},
		"a sealed name":     {"MY_SEALED", "x", "is a secret name"},
		"credential name":   {"GITHUB_TOKEN", "x", "is a secret name"},
		"credential value":  {"PLAIN", "ghp_" + strings.Repeat("a", 36), "looks like a credential"},
		"bad name":          {"1BAD", "x", "not a valid name"},
		"empty value":       {"PLAIN", "  ", "empty value"},
		"secret ref value":  {"PLAIN", "@secret", "seals a secret"},
	} {
		if _, err := SetEnv(c.name, c.value, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to contain %q", name, err, c.want)
		}
	}
	if env := providers.CustomEnv(); len(env) != 0 {
		t.Errorf("refusals wrote %v", env)
	}
}

// An existing @secret reference is the configure menu's to change, not SetEnv's.
func TestSetEnvKeepsSecretReference(t *testing.T) {
	vaulttest.Use(t)
	if err := providers.SetCustomEnv("REFD", providers.SecretRef); err != nil {
		t.Fatal(err)
	}
	if _, err := SetEnv("REFD", "plain", false); err == nil || providers.CustomEnv()["REFD"] != providers.SecretRef {
		t.Errorf("SetEnv over a reference: err %v, bucket %q", err, providers.CustomEnv()["REFD"])
	}
}
