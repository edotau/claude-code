package memory

import (
	"strings"
	"testing"
)

// assistantLine builds one JSONL record shaped like a real Claude Code transcript assistant turn.
func assistantLine(text string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` +
		jsonQuote(text) + `}]}}`
}

func userLine(text string) string {
	return `{"type":"user","message":{"role":"user","content":[{"type":"text","text":` + jsonQuote(text) + `}]}}`
}

func toolResultLine(text string) string {
	return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","text":` +
		jsonQuote(text) + `}]}}`
}

func systemLine(text string) string {
	return `{"type":"system","message":{"role":"system","content":[{"type":"text","text":` + jsonQuote(text) + `}]}}`
}

// jsonQuote escapes text into a JSON string literal without pulling in encoding/json for test fixtures.
func jsonQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestExtractTags_AssistantAllFourKinds(t *testing.T) {
	transcript := strings.Join([]string{
		assistantLine("DECISION: use bbolt — rejected sqlite because no cgo\nBUG: harvest hangs → missing timeout\nNEXT: add retry\nNOTE: vault key lives in keyring"),
	}, "\n")
	tags, err := ExtractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	wantKinds := []string{"DECISION", "BUG", "NEXT", "NOTE"}
	if len(tags) != 4 {
		t.Fatalf("got %d tags, want 4: %+v", len(tags), tags)
	}
	for i, want := range wantKinds {
		if tags[i].Kind != want {
			t.Errorf("tag %d kind = %q, want %q", i, tags[i].Kind, want)
		}
	}
}

func TestExtractTags_UserToolSystemExcluded(t *testing.T) {
	transcript := strings.Join([]string{
		userLine("DECISION: this came from the user and must not be filed"),
		toolResultLine("DECISION: this came from a tool result and must not be filed"),
		systemLine("DECISION: this came from a system message and must not be filed"),
	}, "\n")
	tags, err := ExtractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("got %d tags from non-assistant lines, want 0: %+v", len(tags), tags)
	}
}

func TestExtractTags_OptionalBulletPrefix(t *testing.T) {
	transcript := assistantLine("- DECISION: bulleted tag still counts")
	tags, err := ExtractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Kind != "DECISION" || tags[0].Text != "bulleted tag still counts" {
		t.Fatalf("got %+v", tags)
	}
}

func TestExtractTags_ExactRepeatsDeduped(t *testing.T) {
	transcript := strings.Join([]string{
		assistantLine("NOTE: same fact twice"),
		assistantLine("NOTE: same fact twice"),
	}, "\n")
	tags, err := ExtractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("got %d tags, want 1 after dedup: %+v", len(tags), tags)
	}
}

func TestExtractTags_OverLongCapped(t *testing.T) {
	long := strings.Repeat("x", 500)
	transcript := assistantLine("NOTE: " + long)
	tags, err := ExtractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("got %d tags, want 1: %+v", len(tags), tags)
	}
	if len(tags[0].Text) != tagMaxLen {
		t.Fatalf("tag text len = %d, want %d", len(tags[0].Text), tagMaxLen)
	}
}

func TestExtractTags_OverCountKeepsNewest(t *testing.T) {
	var lines []string
	for i := range tagMaxCount + 5 {
		lines = append(lines, assistantLine("NOTE: fact "+string(rune('a'+i))))
	}
	transcript := strings.Join(lines, "\n")
	tags, err := ExtractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if len(tags) != tagMaxCount {
		t.Fatalf("got %d tags, want %d", len(tags), tagMaxCount)
	}
	// the oldest 5 ("fact a".."fact e") must have been dropped, keeping the newest run.
	if tags[0].Text != "fact "+string(rune('a'+5)) {
		t.Fatalf("first kept tag = %q, want the 6th written", tags[0].Text)
	}
}

func TestExtractTags_MalformedLineSkipped(t *testing.T) {
	transcript := strings.Join([]string{
		"{not valid json",
		assistantLine("NOTE: survives the bad line before it"),
	}, "\n")
	tags, err := ExtractTags(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Text != "survives the bad line before it" {
		t.Fatalf("got %+v", tags)
	}
}

func TestExtractTags_EmptyTranscript(t *testing.T) {
	tags, err := ExtractTags(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("got %d tags from an empty transcript, want 0", len(tags))
	}
}
