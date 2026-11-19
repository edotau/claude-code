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

func TestNormalizeDocCollapses(t *testing.T) {
	doc := map[string]any{"permissions": map[string]any{"allow": []any{"a", "a", "b"}}}
	got, changed, err := normalizeDoc(doc)
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	want := map[string]any{"permissions": map[string]any{"allow": []any{"a", "b"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, none, _ := normalizeDoc(want); len(none) != 0 {
		t.Fatalf("clean doc reported %v", none)
	}
}
