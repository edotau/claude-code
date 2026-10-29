package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/schemas"
)

// TestAuditLogSchemaStd5 pins: unstamped lines read, new lines carry schema 1, schema-2 lines are skipped.
func TestAuditLogSchemaStd5(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	found := false
	for _, f := range schemas.All() {
		found = found || f.Name == "audit"
	}
	if !found {
		t.Error(`schemas.All() has no "audit" row`)
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	seed := `{"time":"` + ts + `","action":"model_pin","resource":"old"}` + "\n" +
		`{"schema":2,"time":"` + ts + `","action":"model_pin","resource":"future"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(File()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(File(), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	Log(KeyRotate, "new", "")

	events, err := Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var res []string
	for _, e := range events {
		res = append(res, e.Resource)
	}
	if strings.Join(res, ",") != "old,new" {
		t.Errorf("resources = %v, want [old new] (schema 2 skipped)", res)
	}
	b, _ := os.ReadFile(File())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, `"schema":1`) {
		t.Errorf("new line lacks schema 1: %s", last)
	}
}
