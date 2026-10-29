package tools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// An old-shape (bare map) or schema-2 agentversion cache is a miss, and the re-probe writes "schema":1.
func TestAgentVersionCacheShapeMismatchIsMissThenWritesSchema1(t *testing.T) {
	for name, body := range map[string]string{
		"v0 bare map": `{"%KEY%":"fake 6.0.0"}`,
		"schema 2":    `{"schema":2,"versions":{"%KEY%":"fake 6.0.0"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			bin, counter := probeFixture(t, "echo 'fake 7.1.0'")
			resetMemo()
			key, ok := versionKey(bin)
			if !ok {
				t.Fatal("no key")
			}
			if err := os.MkdirAll(strings.TrimSuffix(versionCacheFile(), "/agentversion.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(versionCacheFile(), []byte(strings.ReplaceAll(body, "%KEY%", key)), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := readVersionCache(); len(got) != 0 {
				t.Fatalf("readVersionCache = %v, want empty (miss)", got)
			}
			if v, err := AgentVersionAt(bin); err != nil || v != "7.1.0" {
				t.Fatalf("AgentVersionAt = %q, %v (stale cache served?)", v, err)
			}
			if n := invocations(t, counter); n != 1 {
				t.Fatalf("probe ran %d times, want 1", n)
			}
			raw, err := os.ReadFile(versionCacheFile())
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) != nil || string(doc["schema"]) != "1" {
				t.Fatalf("rewritten cache = %s, want schema 1", raw)
			}
		})
	}
}
