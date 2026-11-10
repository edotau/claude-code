package opencode

import (
	"context"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/memory"
)

// TestOpencodeChildMarksHostedRuns: a hook host adds the hosted marker; none (memory child) leaves env and argv alone.
func TestOpencodeChildMarksHostedRuns(t *testing.T) {
	bin, argv, dir := "/bin/echo", []string{"run", "hi"}, t.TempDir()
	req := agent.RunOptions{WorkDir: dir, SafeMode: true}
	cmd, _, host, err := opencodeChild(context.Background(), "opencode", bin, []string{"A=1"}, argv, "hi", req)
	if err != nil || host == nil {
		t.Fatalf("opencodeChild = host %v, err %v; want a safety-gate host", host, err)
	}
	if !slices.Contains(cmd.Env, hostedEnv+"=1") || cmd.Dir != dir {
		t.Errorf("hosted child env %v dir %q, want the hosted marker and WorkDir", cmd.Env, cmd.Dir)
	}
	t.Setenv(memory.UpdateChildEnv, "1")
	cmd, _, host, err = opencodeChild(context.Background(), "opencode", bin, []string{"A=1"}, argv, "hi", req)
	if err != nil || host != nil || slices.Contains(cmd.Env, hostedEnv+"=1") || !slices.Equal(cmd.Args[1:], argv) {
		t.Errorf("memory child: host %v err %v env %v args %v; want no host, no marker, argv as given", host, err, cmd.Env, cmd.Args)
	}
}
