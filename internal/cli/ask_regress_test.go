package cli

import (
	"encoding/json"
	"testing"
)

// A failure after selector resolution publishes the resolved runner, not the raw --agents key.
func TestAskFailureEnvelopeCarriesResolvedRunner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	defs := `{"sec":{"prompt":"Be strict.","runner":"codex"}}`
	args := []string{"--agents", defs, "--agent", "sec", "--format", "json", "--background", "--env", "A=b", "x"}
	code, stdout, stderr := captureOutput(t, func() int { return cmdAsk(args) })
	var env struct{ Agent string }
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || code != 2 {
		t.Fatalf("exit %d, stdout %q, err %v, stderr %q; want a usage envelope", code, stdout, err, stderr)
	}
	if env.Agent != "codex" {
		t.Fatalf("envelope agent = %q, want the resolved runner codex", env.Agent)
	}
}
