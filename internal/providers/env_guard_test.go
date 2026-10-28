package providers

import (
	"strings"
	"testing"
)

func envOverlay(key string) string {
	return `{"providers":{"anthropic":{"e":{"protocol":"anthropic","base_url":"https://e","auth":{"type":"none"},"env":{"` + key + `":"v"}}}}}`
}

func TestValidateRejectsHarnessOwnedEnvKeys(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "HARNESS_PROVIDER",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING",
		"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_FABLE_MODEL"} {
		if _, err := parseOverlay(t, envOverlay(k)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("env key %s accepted (err %v)", k, err)
		}
	}
}

func TestValidateAllowsBenignEnvKeys(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS", "GW_REGION", "DATA_PATH", "CLAUDE_PROFILE_X", "CLAUDE_CODE_FOO_MODEL"} {
		if _, err := parseOverlay(t, envOverlay(k)); err != nil {
			t.Errorf("env key %s rejected: %v", k, err)
		}
	}
}

func TestValidateRejectsCredentialSegmentEnvKeys(t *testing.T) {
	for _, k := range []string{"FOO_HEADERS", "MY_AUTH", "DB_CREDENTIALS", "DB_CREDENTIAL", "DATABRICKS_PAT", "X_BEARER", "X_AUTH_Y"} {
		if _, err := parseOverlay(t, envOverlay(k)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("credential-shaped env key %s accepted (err %v)", k, err)
		}
	}
}
