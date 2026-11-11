package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSchemaRejectsInvalidTools(t *testing.T) {
	setConfigDir(t)
	for _, tools := range []string{"false", "no"} {
		file := filepath.Join(t.TempDir(), "restricted.md")
		if err := writeFile(file, "---\nname: restricted\ntools: "+tools+"\n---\nRestricted prompt.\n"); err != nil {
			t.Fatal(err)
		}
		got, err := LoadSchema(file)
		if err == nil || got != nil {
			t.Fatalf("tools: %s loaded as %+v, %v; want rejection, not inherited tools", tools, got, err)
		}
		if !strings.Contains(err.Error(), "tools must be") {
			t.Fatalf("tools: %s error = %v, want a tools validation error", tools, err)
		}
	}
}

func TestParseAgentsRejectsNullFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agents.json")
	if err := writeFile(file, "null\n"); err != nil {
		t.Fatal(err)
	}
	got, err := ParseAgents(file)
	if err == nil || got != nil {
		t.Fatalf("ParseAgents(null file) = %v, %v; want rejection, not empty definitions", got, err)
	}
	if !strings.Contains(err.Error(), "--agents: top-level value must be a JSON object") {
		t.Fatalf("ParseAgents(null file) error = %v, want an object-shape error", err)
	}
	if err := writeFile(file, "{}\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := ParseAgents(file); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("ParseAgents(empty object) = %v, %v; want valid empty definitions", got, err)
	}
}
