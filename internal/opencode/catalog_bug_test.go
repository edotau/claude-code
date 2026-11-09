package opencode

import (
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestParseMenuImportProviderRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tc := range []struct {
		name      string
		doc       string
		responses bool
	}{
		{"responses npm without suffix", `{"provider":{"databricks":{"npm":"@ai-sdk/openai","models":{"databricks-gpt-6-1-sol":{"name":"GPT 6.1 Sol"}}}}}`, true},
		{"legacy gpt suffix", `{"provider":{"databricks-gpt":{"models":{"databricks-gpt-6-1-sol":{"name":"GPT 6.1 Sol"}}}}}`, true},
		{"chat npm overrides suffix", `{"provider":{"databricks-gpt":{"npm":"@ai-sdk/openai-compatible","models":{"databricks-gpt-6-1-sol":{"name":"GPT 6.1 Sol"}}}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMenuImport([]byte(tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			want := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Label: "GPT 6.1 Sol", Responses: tc.responses}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("provider routing: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseMenuImportLongLine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	label := strings.Repeat("long-label", 8<<10)
	got, err := ParseMenuImport([]byte("databricks-kimi-k3," + label + ",responses\n"))
	if err != nil {
		t.Fatalf("valid menu line over 64 KiB: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parsed %d models, want 1", len(got))
	}
	if got[0].ID != "kimi-k3" || got[0].Label != label || !got[0].Responses {
		t.Fatalf("parsed ID=%q, label matches=%t, responses=%t", got[0].ID, got[0].Label == label, got[0].Responses)
	}
}
