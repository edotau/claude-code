//go:build unix

package proc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// TreeCancel runs cmd in its own group under a per-run env tag; cancel: SIGTERM, grace, then kill descendants and tag.
func TreeCancel(cmd *exec.Cmd, grace time.Duration) {
	OwnGroup(cmd)
	tag := treeTag()
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(slices.Clip(cmd.Env), tag+"=1")
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		kids := Descendants(pid) // snapshot first: a killed parent reparents its children out of reach
		_ = cmd.Process.Signal(syscall.SIGTERM)
		for end := time.Now().Add(grace); time.Now().Before(end) && Alive(pid); {
			time.Sleep(10 * time.Millisecond)
		}
		for _, k := range append(kids, Descendants(pid)...) {
			if Alive(k) {
				_ = SignalGroup(k, syscall.SIGKILL)  // a tool's own group (Pi's bash) is not ours
				_ = syscall.Kill(k, syscall.SIGKILL) // not a group leader: the group signal missed it
			}
		}
		return errors.Join(syscall.Kill(-pid, syscall.SIGKILL), killTagged(tag))
	}
}

// treeTag is a fresh env name no process outside this run's tree carries.
func treeTag() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on unix
	return TreeTagPrefix + hex.EncodeToString(b)
}

// killTagged kills every process whose env holds tag, rescanning while any remain: one may fork mid-sweep.
func killTagged(tag string) error {
	for range 5 {
		pids, err := tagged(tag)
		if err != nil {
			return fmt.Errorf("tree sweep: %w", err)
		}
		if len(pids) == 0 {
			return nil
		}
		for _, p := range pids {
			if hasTag(p, tag) { // re-read just before the kill: a pid that exited since the scan may be reused
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	}
	return nil
}

// untagged is env (nil: this process's) without any TreeCancel tag; nil when there was none to drop.
func untagged(env []string) []string {
	isTag := func(kv string) bool { return strings.HasPrefix(kv, TreeTagPrefix) }
	if env == nil && !slices.ContainsFunc(os.Environ(), isTag) {
		return nil // keep nil: exec then adds PWD for cmd.Dir
	}
	if env == nil {
		env = os.Environ()
	}
	return slices.DeleteFunc(slices.Clone(env), isTag)
}
