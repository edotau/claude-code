package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/defaults"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestWriteRepoDefaultsKeepsSchemaFirst: staged keys land in the file (env variables under env), $schema stays
// first even from a file that had it misplaced, and a staged key outside the schema is refused.
func TestWriteRepoDefaultsKeepsSchemaFirst(t *testing.T) {
	dir := vaulttest.Use(t)
	file := filepath.Join(dir, filepath.FromSlash(RepoDefaultsRel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	o, err := parseObject(defaults.Claude)
	if err != nil {
		t.Fatal(err)
	}
	o.keys = append(o.keys[1:], o.keys[0]) // $schema last
	seed, _ := o.marshal()
	if err := os.WriteFile(file, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	staged := map[string]json.RawMessage{
		"wheelScrollAccelerationEnabled": json.RawMessage("false"),
		"DISABLE_TELEMETRY":              json.RawMessage(`"1"`),
	}
	if _, err := WriteRepoDefaults(map[string]json.RawMessage{"noSuchKey": json.RawMessage("1")}); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := WriteRepoDefaults(staged); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	back, err := parseObject(got)
	if err != nil {
		t.Fatal(err)
	}
	if back.keys[0] != "$schema" {
		t.Errorf("first key = %q, want $schema", back.keys[0])
	}
	if string(back.vals["wheelScrollAccelerationEnabled"]) != "false" || !strings.Contains(string(back.vals["env"]), `"DISABLE_TELEMETRY": "1"`) {
		t.Errorf("staged keys missing:\n%s", got)
	}
}
