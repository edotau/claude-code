package safety

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

type tamperCase struct {
	name, cmd, agentID, dispatchID string
	want                           core.Decision
}

var tamperCases = []tamperCase{
	{"assign empty id", `HARNESS_DISPATCH_ID= claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"env -u id", `env -u HARNESS_DISPATCH_ID claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"unset permissions", `unset HARNESS_DISPATCH_PERMISSIONS; claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"assign max depth", `HARNESS_DISPATCH_MAX_DEPTH=0 claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"assign permissions", `HARNESS_DISPATCH_PERMISSIONS='{}' claude-code codex ask x`, "a1", "", core.Block},
	{"export depth", `export HARNESS_DISPATCH_DEPTH=0`, "a1", "", core.Block},
	{"env -i before claude-code", `env -i PATH=/usr/bin claude-code agents ask --agent codex x`, "a1", "", core.Block},
	{"dispatch leaf unsets", `unset HARNESS_DISPATCH_ID`, "", "d1", core.Block},
	{"env -i before a vendor launcher", `env -i PATH=/usr/bin codex exec x`, "a1", "", core.Block},
	{"env -i before opencode", `env -i opencode run x`, "a1", "", core.Block},
	{"assign after another assignment", `FOO=1 HARNESS_DISPATCH_ID= claude-code agents ask x`, "a1", "", core.Block},
	{"assign through env after -u", `env -u FOO HARNESS_DISPATCH_ID= claude-code agents ask x`, "a1", "", core.Block},
	{"unset inside bash -c", `bash -c "unset HARNESS_DISPATCH_ID; claude-code agents ask x"`, "a1", "", core.Block},
	{"assign inside sh -c", `sh -c 'HARNESS_DISPATCH_DEPTH=0 claude-code agents ask x'`, "a1", "", core.Block},
	{"exec env -u id", `exec env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"nohup env -u id", `nohup env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"time env -u id", `time env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"command env -u id", `command env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"exec env -i launcher", `exec env -i claude-code agents ask x`, "a1", "", core.Block},
	{"nohup env assigns depth", `nohup env HARNESS_DISPATCH_DEPTH=0 claude-code agents ask x`, "a1", "", core.Block},
	{"sudo env -u id", `sudo env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"absolute env -u id", `/usr/bin/env -u HARNESS_DISPATCH_ID claude-code agents ask x`, "a1", "", core.Block},
	{"time go test", `time go test ./...`, "a1", "", core.Allow},
	{"nohup dispatch", `nohup claude-code agents ask --agent codex "x" &`, "a1", "", core.Allow},
	{"exec env unrelated assign", `exec env FOO=1 claude-code agents ask x`, "a1", "", core.Allow},
	{"command -v env", `command -v env`, "a1", "", core.Allow},
	{"time env piped to sort", `time env | sort`, "a1", "", core.Allow},
	{"nice make", `nice -n 10 make test`, "a1", "", core.Allow},
	{"text only", `echo "exec env -u HARNESS_DISPATCH_ID"`, "a1", "", core.Allow},
	{"argument is no assignment", `go test ./... -args HARNESS_DISPATCH_DEPTH=2`, "a1", "", core.Allow},
	{"plain dispatch", `claude-code agents ask --agent codex x`, "a1", "", core.Allow},
	{"echo id", `echo $HARNESS_DISPATCH_ID`, "a1", "", core.Allow},
	{"printenv depth", `printenv HARNESS_DISPATCH_DEPTH`, "a1", "", core.Allow},
	{"unrelated assign", `FOO=1 claude-code agents ask --agent codex x`, "a1", "", core.Allow},
	{"env -i other command", `env -i PATH=/usr/bin ls`, "a1", "", core.Allow},
	{"main session sets depth", `HARNESS_DISPATCH_MAX_DEPTH=5 claude-code agents ask --agent codex x`, "", "", core.Allow},
}

// A leaf may not assign, export, unset or clear a HARNESS_DISPATCH_* variable; the main session may.
func TestDispatchTamper(t *testing.T) {
	t.Setenv("HARNESS_DISPATCH_DEPTH", "")
	for _, c := range tamperCases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HARNESS_DISPATCH_ID", c.dispatchID)
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": "/tmp", "agent_id": c.agentID,
				"tool_input": map[string]string{"command": c.cmd}})
			if r := Scan(core.ParseInput(bytes.NewReader(payload))); r.Decision != c.want {
				t.Fatalf("decision %v, want %v: %s", r.Decision, c.want, r.Reason)
			}
		})
	}
}
