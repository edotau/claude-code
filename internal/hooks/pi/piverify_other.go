//go:build !windows

package pi

import (
	"os/exec"

	"github.com/edotau/claude-code/internal/proc"
)

type stepTree struct{ cmd *exec.Cmd }

// startStep runs cmd in its own group under a tree tag (proc.TreeCancel), so a timeout or cancel kills the tree.
func startStep(cmd *exec.Cmd) (*stepTree, error) {
	proc.TreeCancel(cmd, 0) // after cmd.Env: it appends the tag
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &stepTree{cmd}, nil
}

// reap kills the step's group and tagged kin, even one that left it; pids recycle only on wrap, so none is foreign yet.
func (t *stepTree) reap() { _ = t.cmd.Cancel() }
