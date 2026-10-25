package vendors

import (
	"reflect"
	"slices"
	"testing"
)

func webSearchServers(args []any) map[string]any {
	return map[string]any{
		"web_search":      map[string]any{"command": "claude-code", "args": args},
		"chrome-devtools": map[string]any{"command": "npx"},
	}
}

// web_search survives an empty allow list; other servers are still filtered by allow.
func TestFilterAllowedMCPKeepsWebSearchWithEmptyAllow(t *testing.T) {
	kept, dropped := filterAllowedMCP(webSearchServers([]any{"network", "websearch"}), []string{})
	if _, ok := kept["web_search"]; !ok {
		t.Fatalf("kept = %v, want web_search", kept)
	}
	if _, ok := kept["chrome-devtools"]; ok {
		t.Errorf("chrome-devtools must be filtered: %v", kept)
	}
	if !slices.Equal(dropped, []string{"chrome-devtools"}) {
		t.Errorf("dropped = %v, want [chrome-devtools]", dropped)
	}
	kept, _ = filterAllowedMCP(webSearchServers([]any{"network", "websearch"}), []string{"chrome-devtools"})
	if len(kept) != 2 {
		t.Errorf("allowed server must be kept alongside web_search: %v", kept)
	}
}

// Retired ["web","search"] args are rewritten in the output only; the input map is untouched.
func TestFilterAllowedMCPRewritesOldWebSearchArgs(t *testing.T) {
	in := webSearchServers([]any{"web", "search"})
	kept, _ := filterAllowedMCP(in, nil)
	got := kept["web_search"].(map[string]any)["args"]
	if want := []any{"network", "websearch"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if orig := in["web_search"].(map[string]any)["args"]; !reflect.DeepEqual(orig, []any{"web", "search"}) {
		t.Errorf("input mutated: %v", orig)
	}
	kept, _ = filterAllowedMCP(webSearchServers([]any{"serve", "x"}), nil)
	if got := kept["web_search"].(map[string]any)["args"]; !reflect.DeepEqual(got, []any{"serve", "x"}) {
		t.Errorf("foreign args altered: %v", got)
	}
}

// No web_search registered: nothing is added; a second pass is identical.
func TestFilterAllowedMCPAddsNothingAndIsStable(t *testing.T) {
	in := map[string]any{"chrome-devtools": map[string]any{"command": "npx"}}
	kept, _ := filterAllowedMCP(in, nil)
	if len(kept) != 0 {
		t.Errorf("kept = %v, want empty", kept)
	}
	a, _ := filterAllowedMCP(webSearchServers([]any{"web", "search"}), nil)
	b, _ := filterAllowedMCP(a, nil)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("not stable: %v vs %v", a, b)
	}
}

// A user web_search not running claude-code is an ordinary server: filtered by allow, args untouched.
func TestFilterAllowedMCPForeignWebSearchIsOrdinary(t *testing.T) {
	in := map[string]any{"web_search": map[string]any{"command": "npx", "args": []any{"web", "search"}}}
	kept, dropped := filterAllowedMCP(in, []string{})
	if _, ok := kept["web_search"]; ok || !slices.Equal(dropped, []string{"web_search"}) {
		t.Errorf("npx web_search must be filtered by allow: kept=%v dropped=%v", kept, dropped)
	}
	kept, _ = filterAllowedMCP(in, []string{"web_search"})
	got := kept["web_search"].(map[string]any)["args"]
	if !reflect.DeepEqual(got, []any{"web", "search"}) {
		t.Errorf("foreign args rewritten: %v", got)
	}
}

// A path-qualified claude-code command still counts; an empty command array does not panic.
func TestFilterAllowedMCPHarnessDetection(t *testing.T) {
	in := map[string]any{"web_search": map[string]any{"command": "/usr/bin/claude-code", "args": []any{"web", "search"}}}
	kept, _ := filterAllowedMCP(in, []string{})
	if got := kept["web_search"].(map[string]any)["args"]; !reflect.DeepEqual(got, []any{"network", "websearch"}) {
		t.Errorf("path-qualified claude-code not migrated: %v", got)
	}
	empty := map[string]any{"web_search": map[string]any{"command": []any{}}}
	if kept, _ := filterAllowedMCP(empty, []string{}); len(kept) != 0 {
		t.Errorf("empty command must be an ordinary server: %v", kept)
	}
}
