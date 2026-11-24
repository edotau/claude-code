package ask

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

func TestLineBriefOwn(t *testing.T) {
	got, err := lineBrief(batchLine{Prompt: "do it", Own: []string{"a/b.go"}}, "claude", agent.RunOptions{})
	if err != nil || !strings.Contains(got, `"a/b.go"`) {
		t.Fatalf("brief = %q, %v", got, err)
	}
	if plain, _ := lineBrief(batchLine{Prompt: "do it"}, "claude", agent.RunOptions{}); plain != "do it" {
		t.Fatalf("no contract keys must leave the prompt: %q", plain)
	}
}
