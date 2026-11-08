package agentcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// partArgs is n repeated --part flags followed by tail.
func partArgs(n int, tail ...string) []string {
	var args []string
	for range n {
		args = append(args, "--part", "q")
	}
	return append(args, tail...)
}

func TestDispatchUsageErrorsExit2(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for name, args := range map[string][]string{
		"missing task": {"explore"},
		"no args":      {},
		"unknown role": {"nonesuch", "task"},
		"13 --part":    partArgs(13, "explore", "task"),
		"bad schema":   {"--output-schema", "{not json", "fleet", "task"},
		"schema file":  {"--output-schema", "/nonexistent/schema.json", "fleet", "task"},
	} {
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return dispatchCmd(args) }); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestDispatchErrorMessagesNameTheCause(t *testing.T) {
	vaulttest.UseFreshKey(t)
	_, _, errOut := hosttest.CaptureOutput(t, func() int { return dispatchCmd(partArgs(13, "explore", "task")) })
	if !strings.Contains(errOut, "13 --part") || !strings.Contains(errOut, "cap is 12") {
		t.Errorf("part cap stderr: %q", errOut)
	}
	_, _, errOut = hosttest.CaptureOutput(t, func() int { return dispatchCmd([]string{"--output-schema", "{not json", "fleet", "task"}) })
	if !strings.Contains(errOut, "--output-schema") {
		t.Errorf("bad schema stderr: %q", errOut)
	}
	_, _, errOut = hosttest.CaptureOutput(t, func() int { return dispatchCmd([]string{"nonesuch", "task"}) })
	if !strings.Contains(errOut, `unknown role "nonesuch"`) {
		t.Errorf("unknown role stderr: %q", errOut)
	}
}

// Given a detached leg whose spawn fails (the test binary is never relaunched), Then its record leaves running.
func TestDispatchDetachSpawnFailureFinishesRecord(t *testing.T) {
	vaulttest.UseFreshKey(t)
	legs := []roles.Leg{{Label: "a", Opts: agent.RunOptions{Agent: "claude", Prompt: "p"}}}
	var code int
	hosttest.CaptureOutput(t, func() int {
		code = dispatchDetach(context.Background(), client.NewClient(nil, nil, nil), legs, helptext.Text)
		return code
	})
	if code != 1 {
		t.Fatalf("dispatchDetach = %d, want 1", code)
	}
	recs := job.RecentRecords(5)
	if len(recs) != 1 || recs[0].Status != "error" || recs[0].Error == "" {
		t.Errorf("records = %+v, want one errored record with the cause", recs)
	}
}
