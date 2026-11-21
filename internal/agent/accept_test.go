package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAcceptModel(t *testing.T) {
	vaulttest.Use(t)
	now := time.Now()
	dbx := func(name string) *providers.Provider {
		return &providers.Provider{Name: name, Preset: providers.PresetDatabricks, Host: "https://" + name + ".example.com"}
	}
	warm := router.Served{Profile: "warm", FetchedAt: now, Models: []router.ServedModel{
		{ID: "databricks-claude-opus-5-5", Ready: "READY"}, {ID: "databricks-claude-opus-5-4", Ready: "READY"},
		{ID: "databricks-zeta-9", Ready: "READY"}}}
	if err := router.SaveServed(warm); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, profile, model string
		kind                 UnacceptedKind // "" accepted
		note, near           string         // substring of the note / of the error text
	}{
		{"auto on a cold profile", "cold", "auto", "", "", ""},
		{"served bare name", "warm", "zeta-9", "", "", ""},
		{"served catalog id", "warm", "claude-opus-5-5", "", "", ""},
		{"unserved with nearest ids", "warm", "claude-opus-5-9", UnacceptedUnserved, "", "databricks-claude-opus-5-5"},
		{"unlisted bare name", "warm", "zeta-8", UnacceptedUnserved, "", ""},
		{"cold catalog id", "cold", "claude-opus-5-5", "", "unverified", ""},
		{"cold databricks- id", "cold", "databricks-anything-1", "", "unverified", ""},
		{"cold unknown id", "cold", "zeta-8", UnacceptedCold, "", "claude-code models preflight --live --profile cold"},
		{"prefixed", "warm", "dev:opus", UnacceptedPrefixed, "", "--overlay"},
		{"unlisted id on a row with no listing", "plain", "qwen3-x", "", "unverified", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := dbx(c.profile)
			if c.profile == "plain" {
				p = &providers.Provider{Name: "plain", Protocol: providers.ProtocolOpenAI, BaseURL: "http://127.0.0.1:11434/v1"}
			}
			note, err := AcceptModel("claude", p, c.model, now)
			var ue *UnacceptedError
			if c.kind == "" && err != nil || c.kind != "" && (!errors.As(err, &ue) || ue.Kind != c.kind) {
				t.Fatalf("want kind %q, got %v", c.kind, err)
			}
			if got := note + fmt.Sprint(err); !strings.Contains(got, c.note+c.near) {
				t.Errorf("note %q, err %v: lacks %q", note, err, c.note+c.near)
			}
		})
	}
}

func TestPrefixedRefusalNamesRow(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": {Name: "dev", Preset: providers.PresetDatabricks}}}
	err := PrefixedRefusal(reg, "claude", "dev:claude-opus-5")
	if err == nil || !strings.Contains(err.Error(), `--overlay '{"provider":"databricks","profile":"dev"}' --model claude-opus-5`) {
		t.Errorf("refusal = %v", err)
	}
	if PrefixedRefusal(reg, "claude", "qwen3:30b") != nil || PrefixedRefusal(reg, "claude", "opus") != nil {
		t.Error("plain id refused")
	}
}

// D7: a cold databricks- id a Responses agent runs unverified; only a fresh listing or a chat-only catalog id refuses.
func TestAcceptModelColdCustomIDForCodex(t *testing.T) {
	vaulttest.Use(t)
	cold := &providers.Provider{Name: "cold", Preset: providers.PresetDatabricks, Host: "https://cold.example.com",
		BaseURL: "https://cold.example.com/serving-endpoints", Protocol: providers.ProtocolOpenAI,
		Routes: map[string]string{providers.ProtocolResponses: "/openai/v1"}}
	note, err := AcceptModel("codex", cold, "databricks-my-custom-gpt", time.Now())
	if err != nil || !strings.Contains(note, "unverified") {
		t.Fatalf("note %q, err %v", note, err)
	}
}

// A cold listing's nearest pool is the agent's catalog family; a tasked listing's non-chat endpoints never qualify.
func TestAcceptModelNearestPools(t *testing.T) {
	vaulttest.Use(t)
	now := time.Now()
	tasked := router.Served{Profile: "tasked", FetchedAt: now, Models: []router.ServedModel{
		{ID: "databricks-claude-opus-5-5", Task: chatTask, Ready: "READY"}, {ID: "antibody-risk", Ready: "READY"},
		{ID: "databricks-gte-large-en", Task: "llm/v1/embeddings", Ready: "READY"}}}
	if err := router.SaveServed(tasked); err != nil {
		t.Fatal(err)
	}
	dbx := &providers.Provider{Name: "tasked", Preset: providers.PresetDatabricks, Host: "https://tasked.example.com"}
	for _, model := range []string{"antibody-risk", "databricks-gte-large-en"} {
		_, err := AcceptModel("claude", dbx, model, now)
		var ue *UnacceptedError
		if !errors.As(err, &ue) || slices.Contains(ue.Nearest, "antibody-risk") || slices.Contains(ue.Nearest, "databricks-gte-large-en") {
			t.Errorf("%s: want a refusal naming chat ids only, got %v", model, err)
		}
	}
	cold := &providers.Provider{Name: "cold", Preset: providers.PresetDatabricks, Host: "https://cold.example.com"}
	_, err := AcceptModel("claude", cold, "claud-sonnet-5-5", now)
	if ue, ok := errors.AsType[*UnacceptedError](err); !ok || !slices.Contains(ue.Nearest, "databricks-claude-sonnet-5-5") {
		t.Errorf("cold databricks: want nearest databricks-claude-sonnet-5-5, got %v", err)
	}
}

// agy's <id>-<effort> is a catalog id once the native effort suffix is cut.
func TestAcceptModelGeminiEffortSuffix(t *testing.T) {
	vaulttest.Use(t)
	cold := &providers.Provider{Name: "cold", Preset: providers.PresetDatabricks, Host: "https://cold.example.com"}
	if note, err := AcceptModel("gemini", cold, "gemini-3-8-flash-high", time.Now()); err != nil || !strings.Contains(note, "unverified") {
		t.Fatalf("note %q, err %v", note, err)
	}
}
