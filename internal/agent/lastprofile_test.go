package agent

import (
	"context"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// Given a recorded workspace profile, a Claude launch resolves flag > last-used > pin > registry default.
func TestClaudeProfileResolutionLastUsed(t *testing.T) {
	hermetic(t, "")
	ws := t.TempDir()
	t.Chdir(ws)
	profile := func(o LaunchOptions) string {
		t.Helper()
		plan, err := Claude(context.Background(), o, nil)
		if err != nil {
			t.Fatal(err)
		}
		return plan.Set[providers.EnvSessionProfile]
	}
	if got := profile(LaunchOptions{}); got != "max" {
		t.Fatalf("no record: got %q, want the registry default max", got)
	}
	RecordLastProfile(Plan{Set: map[string]string{providers.EnvSessionProfile: "nvidia"}}, nil)
	if got := profile(LaunchOptions{}); got != "nvidia" {
		t.Errorf("last-used: got %q, want nvidia", got)
	}
	RecordLastProfile(Plan{Set: map[string]string{providers.EnvSessionProfile: "anthropic"}}, []string{"-p", "x"})
	if got := profile(LaunchOptions{}); got != "nvidia" {
		t.Errorf("headless run recorded: got %q, want nvidia", got)
	}
	if got := profile(LaunchOptions{Provider: "anthropic"}); got != "anthropic" {
		t.Errorf("flag: got %q, want anthropic", got)
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: "max"}); err != nil {
		t.Fatal(err)
	}
	if got := profile(LaunchOptions{}); got != "nvidia" {
		t.Errorf("pin set: got %q, want nvidia (this workspace beats the pin)", got)
	}
	if got := providers.Pin(providers.PinProfile); got != "max" {
		t.Errorf("recording must not touch the pin, got %q", got)
	}
	if err := ClearLastProfiles(); err != nil {
		t.Fatal(err)
	}
	if got := profile(LaunchOptions{}); got != "max" {
		t.Errorf("after switch clears the cache: got %q, want the pin max", got)
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: ""}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastProfileFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := profile(LaunchOptions{}); got != "max" {
		t.Errorf("corrupt file: got %q, want the default", got)
	}
}
