package memhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/settings"
)

func agentIn(t *testing.T, payload string) *core.Input {
	t.Helper()
	return core.ParseInput(strings.NewReader(payload))
}

// fakeOpencode puts an `opencode` script running body first on PATH and returns a directory to use as cwd.
func fakeOpencode(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestAgentInputClaudePassesThrough(t *testing.T) {
	in := agentIn(t, `{"session_id":"s9","cwd":"/r","transcript_path":"/t.jsonl"}`)
	if out := AgentInput(in); out != in {
		t.Fatal("a Claude-shaped input must come back untouched")
	}
}

func TestAgentInputFailOpenReturnsInput(t *testing.T) {
	cwd := fakeOpencode(t, "exit 1")
	missing := filepath.Join(t.TempDir(), "events.jsonl")
	for name, payload := range map[string]string{
		"opencode export fails":  `{"session_id":"ses_x2","cwd":"` + cwd + `","harness_agent":"opencode"}`,
		"copilot events missing": `{"sessionId":"stable-copilot","cwd":"/r","transcriptPath":"` + missing + `"}`,
	} {
		if in := agentIn(t, payload); AgentInput(in) != in {
			t.Errorf("%s: a failed load must return the same input", name)
		}
	}
}

func TestAgentKind(t *testing.T) {
	cases := []struct {
		name, env, payload, want string
	}{
		{"env opencode", memory.SourceOpencode, `{"session_id":"s","transcript_path":"/t"}`, memory.SourceOpencode},
		{"harness_agent opencode", "", `{"session_id":"s","harness_agent":"opencode"}`, memory.SourceOpencode},
		{"copilot with transcript", "", `{"sessionId":"c","transcriptPath":"/e.jsonl"}`, memory.SourceCopilot},
		{"copilot without transcript", "", `{"sessionId":"c"}`, ""},
		{"plain claude", "", `{"session_id":"s","transcript_path":"/t"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(settings.AgentEnv, tc.env)
			if got := agentKind(agentIn(t, tc.payload)); got != tc.want {
				t.Fatalf("agentKind = %q, want %q", got, tc.want)
			}
		})
	}
}
