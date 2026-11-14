package agent

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
)

func TestExplainPermissionReplay(t *testing.T) {
	for _, call := range []Rule{{Tool: "Bash", Arg: "make lint"}, {Tool: "Read", Arg: "/tmp/a'b"}} {
		assertPermissionReplay(t, call)
	}
	deny := Rule{Tool: "Bash", Arg: "rm:*"}
	p := Permissions{Mode: "plan", Allow: []Rule{{Tool: "Bash"}}, Deny: []Rule{deny}}
	d := p.Explain("Bash", "echo ok && rm file")
	if d.Rule != deny || d.Source != "permissions.deny" || d.Layer != "permissions" || strings.Contains(d.Remedy, "--allow") || d.Replay != "" {
		t.Fatalf("deny lost provenance or suggested an allow: %+v", d)
	}
	if d := p.Explain("Bash", "echo ok"); d != (Denial{}) {
		t.Fatalf("allowed call explained as denial: %+v", d)
	}
	for _, cmd := range []string{"echo hi > file", "echo $(date)"} {
		if d := (Permissions{Mode: "plan"}).Explain("Bash", cmd); strings.Contains(d.Remedy, "--allow") {
			t.Fatalf("ineffective allow: %+v", d)
		}
	}
}

func TestExplainRemedyNeverWidens(t *testing.T) {
	p := Permissions{Mode: "plan"}
	for _, call := range []Rule{
		{Tool: "Bash", Arg: "rm -rf *"}, {Tool: "Bash", Arg: "rm:*"}, {Tool: "Read", Arg: "/tmp/a?b"},
		{Tool: "Read", Arg: "/tmp/**"}, {Tool: "Bash", Arg: "cd:/tmp"}, {Tool: "WebFetch", Arg: "domain:example.com"},
	} {
		if d := p.Explain(call.Tool, call.Arg); strings.Contains(d.Remedy, "--allow") || d.Replay != "" ||
			!strings.Contains(d.Remedy, "write the rule by hand") {
			t.Fatalf("%v: pattern call got an allow remedy: %+v", call, d)
		}
	}
	d := p.Explain("Bash", "rm -rf /tmp/x")
	rule, err := ParseRule(strings.Trim(strings.TrimPrefix(d.Remedy, "--allow "), "'"))
	if err != nil {
		t.Fatal(err)
	}
	allowed, _ := resolvePermissions(p, Permissions{Allow: []Rule{rule}})
	if allowed.Decide("Bash", "rm -rf /tmp/x") != Allow || allowed.Decide("Bash", "rm -rf "+"/") == Allow {
		t.Fatalf("remedy %q is not exactly the denied call", d.Remedy)
	}
}

func TestCapabilityRefusal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		req        RunOptions
		capability string
	}{
		{"gemini", RunOptions{NoTools: true}, "--no-tools"},
		{"claude", RunOptions{Tools: true, NoTools: true}, "--tools"},
		{"copilot", RunOptions{SafeMode: true}, "--safe-mode"},
	} {
		err := CheckCapabilities(tc.name, tc.req)
		var failure *retry.Error
		if !errors.As(err, &failure) || failure.Kind != retry.KindFatal || !strings.Contains(err.Error(), tc.capability) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if err := CheckCapabilities("claude", RunOptions{}); err != nil {
		t.Fatal(err)
	}
}

func assertPermissionReplay(t *testing.T, call Rule) {
	t.Helper()
	p := Permissions{Mode: "plan"}
	d := p.Explain(call.Tool, call.Arg)
	out, err := exec.Command("sh", "-c", "set -- "+d.Remedy+"; printf '%s' \"$2\"").Output()
	if err != nil {
		t.Fatal(err)
	}
	rule, err := ParseRule(string(out))
	if err != nil {
		t.Fatal(err)
	}
	repaired, _ := resolvePermissions(p, Permissions{Allow: []Rule{rule}})
	if d.Replay != d.Remedy || repaired.Decide(call.Tool, call.Arg) != Allow {
		t.Fatalf("unusable replay: %+v", d)
	}
}
