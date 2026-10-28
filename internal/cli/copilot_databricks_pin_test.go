package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestValidateDatabricksCopilotPin(t *testing.T) {
	cliScratch(t)
	served := map[string][]string{
		"dev": {"databricks-kimi-k3", "databricks-gpt-6-1-sol"},
	}
	for _, tc := range []struct {
		model   string
		wantErr bool
		hint    string
	}{
		{model: "kimi-k3"},
		{model: "databricks-kimi-k3"},
		{model: "kimi-k4", wantErr: true},
		{model: "kimi", wantErr: true, hint: "did you mean: databricks-kimi-k3"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			_, err := validateDatabricksCopilotPin(&providers.Registry{}, "dev", tc.model, served)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateDatabricksCopilotPin(%q) error = %v, wantErr %v", tc.model, err, tc.wantErr)
			}
			if tc.hint != "" && (err == nil || !strings.Contains(err.Error(), tc.hint)) {
				t.Errorf("error = %v, want it to contain %q", err, tc.hint)
			}
		})
	}
}
