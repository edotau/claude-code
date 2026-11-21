package copilot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func TestFamilyWordPicksHighestRankedAvailable(t *testing.T) {
	ids := []string{"gpt-6-luna", "gpt-6-astra", "gpt-6.1-sol", "gemini-4-hydrogen", "gemini-4-argon", "grok-4.6", "grok-4.7"}
	for family, want := range map[string]string{"gpt": "gpt-6.1-sol", "gemini": "gemini-4-argon", "grok": "grok-4.7"} {
		got, err := reviewModel(family, "", ids)
		if err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %s", family, got, err, want)
		}
	}
}

func TestFamilyUnknownWordKeepsExistingValidation(t *testing.T) {
	got, err := ReviewModel(context.Background(), "unknown-family", "claude-opus-5-5")
	if err != nil || got != "unknown-family" {
		t.Fatalf("unknown model changed before existing validation: %q, %v", got, err)
	}
}

func TestFamilyConcreteIDUntouched(t *testing.T) {
	got, err := ReviewModel(context.Background(), "gpt-6-luna", "gpt-6.1-sol")
	if err != nil || got != "gpt-6-luna" {
		t.Fatalf("concrete model changed: %q, %v", got, err)
	}
}

func TestFamilyDefaultAvoidsAuthor(t *testing.T) {
	ids := []string{"claude-opus-5.5", "gpt-6.1-sol", "gemini-4-argon", "grok-4.7"}
	for _, author := range []string{"gpt-6.1-sol", "databricks-claude-sonnet-5-5", "gemini-4-argon"} {
		got, err := reviewModel("", author, ids)
		if err != nil || got == "" || modelFamily(got) == modelFamily(author) || modelFamily(got) == "claude" {
			t.Fatalf("author %s: default %q, %v", author, got, err)
		}
	}
	if got, err := reviewModel("", "", ids); got != "" || err != nil {
		t.Fatalf("unknown author changed default: %q, %v", got, err)
	}
}

func TestResolveModelWordUsesSealedSnapshotOnly(t *testing.T) {
	old := effortModels
	t.Cleanup(func() { effortModels = old })
	effortModels = func() []SDKModel {
		return []SDKModel{{ID: "gpt-6-luna", Enabled: true}, {ID: "gpt-6.1-sol", Enabled: true}, {ID: "gpt-6-astra"}}
	}
	for word, want := range map[string]string{"gpt": "gpt-6.1-sol", "gpt-6-luna": "gpt-6-luna", "auto": "auto"} {
		if got, err := copilotAgent.ResolveModelWord(t.Context(), word); err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %s", word, got, err, want)
		}
	}
	if _, err := copilotAgent.ResolveModelWord(t.Context(), "grok"); err == nil {
		t.Fatal("a family with no catalog id must fail like an unknown model")
	}
	effortModels = func() []SDKModel { return nil }
	if got, err := copilotAgent.ResolveModelWord(t.Context(), "gpt"); err != nil || got != "gpt" {
		t.Fatalf("no snapshot must leave the word: %q, %v", got, err)
	}
}

func TestColdModelWordNamesTheFix(t *testing.T) {
	if err := copilotAgent.ColdModelWord("gemini"); err == nil || !strings.Contains(err.Error(), "copilot catalog") {
		t.Fatalf("family word: %v", err)
	}
	if err := copilotAgent.ColdModelWord("gpt-6-luna"); err != nil {
		t.Fatalf("concrete id: %v", err)
	}
}

func TestResolveModelAppliesFamilyWord(t *testing.T) {
	old := effortModels
	t.Cleanup(func() { effortModels = old })
	effortModels = func() []SDKModel {
		return []SDKModel{{ID: "gpt-6-luna", Enabled: true}, {ID: "gpt-6.1-sol", Enabled: true}}
	}
	base, err := os.ReadFile("../providers/defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Parse(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := agent.ResolveModel(reg, agent.ModelQuery{Agent: "copilot", Model: "gpt", Profile: "copilot"}, time.Now())
	if err != nil || r.Model != "gpt-6.1-sol" {
		t.Fatalf("route = %+v, %v; want gpt-6.1-sol", r, err)
	}
}
