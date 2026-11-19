package askspec

import (
	"slices"
	"strings"
	"testing"
)

func testSchema() Schema {
	return Schema{Fields: []Field{
		{Name: "agent", Kind: "string"},
		{Name: "fallback", Kind: "string", Choices: []string{"none", "pin", "agents", "all"}},
		{Name: "model", Kind: "string"},
	}}
}

func TestArgvNearestKey(t *testing.T) {
	s := testSchema()
	argv, prompt, err := s.Argv([]byte(`{"prompt":"hi","model":"opus","agent":"codex"}`))
	if want := []string{"--agent", "codex", "--model", "opus"}; err != nil || prompt != "hi" || !slices.Equal(argv, want) {
		t.Fatalf("argv = %q prompt %q err %v, want %q", argv, prompt, err, want)
	}
	if _, _, err := s.Argv([]byte(`{"agnet":"codex"}`)); err == nil || !strings.Contains(err.Error(), `"agent"`) {
		t.Fatalf("unknown key err = %v, want one naming agent", err)
	}
	if _, _, err := s.Argv([]byte(`{"fallback":"bogus"}`)); err == nil || !strings.Contains(err.Error(), "--fallback") {
		t.Fatalf("bad choice err = %v, want one naming --fallback", err)
	}
}
