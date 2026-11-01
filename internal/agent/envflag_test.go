package agent

import (
	"strings"
	"testing"
)

func TestValidEnvNameRefusesScrubbedNames(t *testing.T) {
	for _, bad := range []string{"HARNESS_VAULT_KEY", "HARNESS_ANYTHING", "ANTHROPIC_API_KEY", "DATABRICKS_HOST", "CLAUDE_PROFILE", "FOO_TOKEN"} {
		if err := validEnvName(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, ok := range []string{"HTTP_PROXY", "ANTHROPIC_BASE_URL", "FOO"} {
		if err := validEnvName(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	if err := ValidateEnv([]string{"sk-supersecret"}); err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Errorf("a no-= entry must be refused without echoing it: %v", err)
	}
}
