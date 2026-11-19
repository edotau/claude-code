package claude

import (
	"reflect"
	"testing"
)

func TestMergeDocUserOverHarness(t *testing.T) {
	base := map[string]any{
		"autoMode": map[string]any{"allow": []any{"h1"}},
		"theme":    "harness",
	}
	user := map[string]any{
		"autoMode": map[string]any{"allow": []any{"u1"}},
		"theme":    "user",
		"model":    "m",
	}
	got := mergeDoc(base, user)
	want := map[string]any{
		"autoMode": map[string]any{"allow": []any{"h1", "u1"}},
		"theme":    "user",
		"model":    "m",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeDoc = %v, want %v", got, want)
	}
	if base["theme"] != "harness" {
		t.Fatal("base mutated")
	}
}
