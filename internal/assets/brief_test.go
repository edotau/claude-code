package assets

import (
	"strings"
	"testing"
)

// Only a pi brief tells the model to finish with the typed handback call.
func TestBriefBodyPiHandback(t *testing.T) {
	in := BriefFrame{Agent: "pi", Verify: "v", Report: "r", Task: "T", OwnedLine: `["x.go"]`, ReportsDir: "/R", Rtk: true}
	got := BriefBody(in)
	if !strings.Contains(got, "calling the `handback` tool once, last") {
		t.Fatal("pi brief lacks the handback instruction")
	}
	in.Agent = "codex"
	if strings.Contains(BriefBody(in), "handback") {
		t.Fatal("another agent's brief names handback")
	}
}
