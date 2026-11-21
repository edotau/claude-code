package agent

import (
	"errors"
	"fmt"
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
	} {
		t.Run(c.name, func(t *testing.T) {
			note, err := AcceptModel("claude", dbx(c.profile), c.model, now)
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
