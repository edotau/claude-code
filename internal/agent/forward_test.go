package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestForwardEvents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const parent = "20261123T010000.000-codex"
	child := Record{ID: "20261123T010000.001-codex", Parent: parent, Agent: "codex", Status: "ok"}
	if err := child.Save(); err != nil {
		t.Fatal(err)
	}
	body := "{\"type\":\"start\",\"status\":\"running\"}\n" +
		"{\"type\":\"tool_use\",\"tool\":{\"name\":\"Read\"},\"text\":\"one\\n two\"}\n" +
		"{\"type\":\"text\",\"text\":\"answer\"}\n" +
		"{\"type\":\"progress\"}\n{\"type\":\"result\",\"text\":\"answer\"}\n"
	if err := os.WriteFile(child.EventsPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cancelled", "stop", "skip"} {
		t.Run(mode, func(t *testing.T) {
			var lines []string
			log := func(kind, text string) { lines = append(lines, kind+": "+text) }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "stop" {
				stop := StartForward(ctx, parent, "", log)
				stop()
				stop()
			} else {
				cancel()
				skip := ""
				if mode == "skip" {
					skip = child.ID
				}
				ForwardEvents(ctx, parent, skip, log)
			}
			want := []string{"start: via " + child.ID + ": running", "tool_use: via " + child.ID + ": Read one two"}
			if mode == "skip" {
				want = nil
			}
			if !slices.Equal(lines, want) || strings.Contains(strings.Join(lines, "\n"), "answer") {
				t.Fatalf("forwarded = %q, want %q without answer text", lines, want)
			}
		})
	}
}
