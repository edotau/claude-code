package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func TestAgentProfileAndOnOffVerbs(t *testing.T) {
	useVaultTest(t)

	// copilot off pins COPILOT_ACTIVE=off and names the pin.
	code, out, errOut := captureOutput(t, func() int { return cmdAgentOnOff("copilot", false, nil) })
	if code != 0 || !strings.Contains(out, providers.PinCopilotActive) {
		t.Fatalf("copilot off: code=%d out=%q err=%q", code, out, errOut)
	}
	if got := providers.Pin(providers.PinCopilotActive); got != agent.AgentOff {
		t.Fatalf("COPILOT_ACTIVE = %q, want off", got)
	}

	// profile with no args reports the off state.
	code, out, _ = captureOutput(t, func() int { return cmdAgentProfile("copilot", nil) })
	if code != 0 || !strings.Contains(out, "off") {
		t.Fatalf("copilot profile: code=%d out=%q", code, out)
	}

	// on is a profile: with none recorded the agent stays off, and setting one switches it on.
	code, _, errOut = captureOutput(t, func() int { return cmdAgentProfile("copilot", []string{"personal"}) })
	if code != 0 || !agent.AgentActive("copilot") {
		t.Fatalf("copilot profile personal: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}
	code, _, errOut = captureOutput(t, func() int { return cmdAgentOnOff("copilot", false, nil) })
	if code != 0 || agent.AgentActive("copilot") {
		t.Fatalf("copilot off with a profile: code=%d active=%v err=%q", code, agent.AgentActive("copilot"), errOut)
	}

	// claude has no active pin.
	code, _, errOut = captureOutput(t, func() int { return cmdAgentOnOff("claude", false, nil) })
	if code == 0 || !strings.Contains(errOut, "always on") {
		t.Fatalf("claude off: code=%d err=%q", code, errOut)
	}

	// unknown profile is refused and leaves the pin unset.
	code, _, _ = captureOutput(t, func() int { return cmdAgentProfile("codex", []string{"nosuchprofile"}) })
	if code == 0 || providers.Pin(providers.PinCodexProfile) != "" {
		t.Fatalf("codex profile nosuchprofile: code=%d pin=%q", code, providers.Pin(providers.PinCodexProfile))
	}

	code, _, errOut = captureOutput(t, func() int { return cmdAgentProfile("codex", []string{"openai"}) })
	if code != 0 || providers.Pin(providers.PinCodexProfile) != "openai" {
		t.Fatalf("codex profile openai: code=%d pin=%q err=%q", code, providers.Pin(providers.PinCodexProfile), errOut)
	}

	code, _, errOut = captureOutput(t, func() int { return cmdAgentProfile("codex", []string{"--clear"}) })
	if code != 0 || providers.Pin(providers.PinCodexProfile) != "" {
		t.Fatalf("codex profile --clear: code=%d pin=%q err=%q", code, providers.Pin(providers.PinCodexProfile), errOut)
	}
}
