package ask

import (
	"strings"
	"testing"
)

func TestLineBriefOwn(t *testing.T) {
	got, err := lineBrief(batchLine{Prompt: "do it", Own: []string{"a/b.go"}}, "claude", false)
	if err != nil || !strings.Contains(got, `"a/b.go"`) {
		t.Fatalf("brief = %q, %v", got, err)
	}
	if plain, _ := lineBrief(batchLine{Prompt: "do it"}, "claude", false); plain != "do it" {
		t.Fatalf("no contract keys must leave the prompt: %q", plain)
	}
}
