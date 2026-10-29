package hooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// TestCodexRoutingSchemaStd5 pins: unstamped lines read, new lines carry schema 1, schema-2 lines are skipped.
func TestCodexRoutingSchemaStd5(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(hookspec.AgentEnv, VendorCodex)
	found := false
	for _, f := range schemas.All() {
		found = found || f.Name == "codex-routing"
	}
	if !found {
		t.Error(`schemas.All() has no "codex-routing" row`)
	}

	path := filepath.Join(paths.StateDir(), "codex", fmt.Sprintf("routing-%x.jsonl", sha256.Sum256([]byte("native-session"))))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"event":"session-start","session_id":"native-session","matches_router_decision":null,"at":1}` + "\n" +
		`{"schema":2,"event":"child-start","session_id":"native-session","agent_id":"future","matches_router_decision":null,"at":2}` + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	rows, err := readCodexRoutes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Event != "session-start" {
		t.Fatalf("rows = %+v, want only the unstamped session-start (schema 2 skipped)", rows)
	}

	runCodexAuditHandler(t, CodexRoutingStart, map[string]any{"model": codexRouteWireModel})
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	var last map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
		t.Fatal(err)
	}
	if last["schema"] != float64(1) {
		t.Errorf(`new line "schema" = %v, want 1 (line %s)`, last["schema"], lines[len(lines)-1])
	}
}
