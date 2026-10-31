package agent

import "testing"

// Catalog limits only lower the gated window and the output cap; unknown models keep today's values.
func TestCapsLowerOnly(t *testing.T) {
	for _, tc := range []struct {
		id             string
		window         int
		wantCtx, wantO int
	}{
		{"databricks-claude-sonnet-5-5", 200_000, 200_000, copilotMaxOutputTokens},
		{"claude-opus-5-5", 2_000_000, 1_000_000, copilotMaxOutputTokens},
		{"databricks-gpt-5", 200_000, 200_000, copilotMaxOutputTokens},
		{"not-a-model", 50_000, 50_000, copilotMaxOutputTokens},
	} {
		if ctx, out := copilotCaps(tc.window, tc.id); ctx != tc.wantCtx || out != tc.wantO {
			t.Errorf("copilotCaps(%d, %q) = %d, %d; want %d, %d", tc.window, tc.id, ctx, out, tc.wantCtx, tc.wantO)
		}
	}
}
