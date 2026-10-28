package translate

import (
	"strings"
	"testing"
)

func TestReasoningFamilyBody(t *testing.T) {
	in := `{"max_tokens":8,"temperature":1,"top_p":0.5,"messages":[{"role":"user","content":"x"}]}`
	for model, reasoning := range map[string]bool{
		"gpt-6-luna": true, "databricks-gpt-6-1-sol": true, "openai/gpt-5": true, "o3": true, "gpt-oss-120b": false, "deepseek-chat": false,
	} {
		out, _, err := ToChatRequest([]byte(in), model)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if got := strings.Contains(s, "max_completion_tokens"); got != reasoning {
			t.Errorf("%s: max_completion_tokens present=%v want %v: %s", model, got, reasoning, s)
		}
		if got := strings.Contains(s, "temperature") || strings.Contains(s, "top_p"); got == reasoning {
			t.Errorf("%s: sampling params present=%v with reasoning=%v: %s", model, got, reasoning, s)
		}
	}
}
