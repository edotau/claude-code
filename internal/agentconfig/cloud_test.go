package agentconfig

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCheckCloudAgent(t *testing.T) {
	for agent, wantErr := range map[string]bool{"claude": false, "codex": false, "copilot": true, "gemini": true, "opencode": true} {
		err := Overlay{Cloud: true}.CheckCloudAgent(agent)
		if (err != nil) != wantErr {
			t.Errorf("%s: err=%v, want refusal=%v", agent, err, wantErr)
		}
		if err := (Overlay{}).CheckCloudAgent(agent); err != nil {
			t.Errorf("%s without cloud: %v", agent, err)
		}
	}
}

func TestCloudGate(t *testing.T) {
	vaulttest.Use(t)
	tests := []struct {
		name  string
		env   proc.Values
		o     Overlay
		agent string
		want  string // "" = allowed
	}{
		{"opted in", nil, Overlay{Cloud: true}, "claude", ""},
		{"no field", nil, Overlay{}, "codex", `relaunch with overlay {"cloud":true}`},
		{"nested", proc.Values{proc.DispatchIDEnv: "d1"}, Overlay{Cloud: true}, "codex", "outermost ask"},
		{"nested by depth alone", proc.Values{proc.DispatchDepthEnv: "1"}, Overlay{Cloud: true}, "codex", "outermost ask"},
		{"databricks provider", nil, Overlay{Cloud: true, Provider: "databricks"}, "claude", "own login only"},
		{"other profile", nil, Overlay{Cloud: true, Profile: "no-such-profile"}, "codex", "own login only"},
		{"own provider", nil, Overlay{Cloud: true, Provider: "openai"}, "codex", ""},
	}
	for _, tc := range tests {
		ctx := t.Context()
		if tc.env != nil {
			ctx = proc.WithValues(ctx, tc.env)
		}
		err := CloudGate(ctx, tc.o, tc.agent, "lane", "the repo")
		switch {
		case tc.want == "" && err != nil, tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
}
