package agentcmd

import (
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func TestAgentProfileAndOnOffVerbs(t *testing.T) {
	vaulttest.UseFreshKey(t)

	// copilot off leaves its record without a profile.
	code, out, errOut := hosttest.CaptureOutput(t, func() int { return cmdAgentOnOff("copilot", false, nil) })
	if code != 0 || !strings.Contains(out, "copilot: off") {
		t.Fatalf("copilot off: code=%d out=%q err=%q", code, out, errOut)
	}
	if got := providers.AgentRecord("copilot").Profile; got != "" {
		t.Fatalf("copilot profile = %q, want none", got)
	}

	// profile with no args reports the off state.
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdAgentProfile("copilot", nil) })
	if code != 0 || !strings.Contains(out, "off") {
		t.Fatalf("copilot profile: code=%d out=%q", code, out)
	}

	checkCopilotProfileActivation(t)
	checkClaudeAndCodexProfiles(t)
}

func checkCopilotProfileActivation(t *testing.T) {
	t.Helper()
	// on is a profile: with none recorded the agent stays off, and setting one switches it on.
	code, _, errOut := hosttest.CaptureOutput(t, func() int { return cmdAgentProfile("copilot", []string{"personal"}) })
	if code != 0 || !agent.AgentActive("copilot") {
		t.Fatalf("copilot profile personal: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}
	code, _, errOut = hosttest.CaptureOutput(t, func() int { return cmdAgentOnOff("copilot", false, nil) })
	if code != 0 || agent.AgentActive("copilot") {
		t.Fatalf("copilot off with a profile: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}
}

func checkClaudeAndCodexProfiles(t *testing.T) {
	t.Helper()
	// claude has no active pin.
	code, _, errOut := hosttest.CaptureOutput(t, func() int { return cmdAgentOnOff("claude", false, nil) })
	if code == 0 || !strings.Contains(errOut, "always on") {
		t.Fatalf("claude off: code=%d err=%q", code, errOut)
	}

	// unknown profile is refused and leaves the pin unset.
	code, _, _ = hosttest.CaptureOutput(t, func() int { return cmdAgentProfile("codex", []string{"nosuchprofile"}) })
	if code == 0 || providers.AgentRecord("codex").Profile != "" {
		t.Fatalf("codex profile nosuchprofile: code=%d pin=%q", code, providers.AgentRecord("codex").Profile)
	}

	code, _, errOut = hosttest.CaptureOutput(t, func() int { return cmdAgentProfile("codex", []string{"openai"}) })
	if code != 0 || providers.AgentRecord("codex").Profile != "openai" {
		t.Fatalf("codex profile openai: code=%d pin=%q err=%q", code, providers.AgentRecord("codex").Profile, errOut)
	}

	code, _, errOut = hosttest.CaptureOutput(t, func() int { return cmdAgentProfile("codex", []string{"--clear"}) })
	if code != 0 || providers.AgentRecord("codex").Profile != "chatgpt" {
		t.Fatalf("codex profile --clear: code=%d pin=%q err=%q", code, providers.AgentRecord("codex").Profile, errOut)
	}
}
