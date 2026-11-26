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

	"golang.org/x/sys/unix"
)

// getsid is the session id of pid, -1 when unreadable.
func getsid(pid int) int {
	s, err := unix.Getsid(pid)
	if err != nil {
		return -1
	}
	return s
}

// TreeCancel runs cmd in its own group under a per-run env tag; cancel: SIGTERM, grace, then kill descendants and tag.
func TreeCancel(cmd *exec.Cmd, grace time.Duration) {
	OwnGroup(cmd)
	tag := treeTag()
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// a marker inherited from a detached parent must not make this run's tool sessions look detached
	cmd.Env = append(slices.DeleteFunc(slices.Clone(cmd.Env), func(kv string) bool {
		return strings.HasPrefix(kv, detachedMarker+"=")
	}), tag+"=1")
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		kids := Descendants(pid) // snapshot first: a killed parent reparents its children out of reach
		_ = cmd.Process.Signal(syscall.SIGTERM)
		for end := time.Now().Add(grace); time.Now().Before(end) && Alive(pid); {
			time.Sleep(10 * time.Millisecond)
		}
		for _, k := range killable(append(kids, Descendants(pid)...), pid, getsid, func(p int) bool {
			return isDetached(p, func(p int) bool { return hasTag(p, detachedMarker) }, envUnreadable)
		}) {
			if Alive(k) {
				_ = SignalGroup(k, syscall.SIGKILL)  // a tool's own group (Pi's bash) is not ours
				_ = syscall.Kill(k, syscall.SIGKILL) // not a group leader: the group signal missed it
			}
		}
		return errors.Join(syscall.Kill(-pid, syscall.SIGKILL), killTagged(tag))
	}
}

// ReapGroup SIGKILLs a dead leader's group until it is empty: a child forked during the cancel sweep escapes it.
func ReapGroup(pgid int, limit time.Duration) bool {
	if pgid <= 1 {
		return true
	}
	for end := time.Now().Add(limit); ; time.Sleep(time.Millisecond) {
		if errors.Is(syscall.Kill(-pgid, syscall.SIGKILL), syscall.ESRCH) {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// TreeSweep kills what outlived a finished TreeCancel run, its group and its tagged processes; nil once none is left.
func TreeSweep(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil // it never started
	}
	tag := TreeTag(cmd)
	if tag == "" {
		return errors.New("tree sweep: cmd has no TreeCancel tag")
	}
	group := true // signalled only until the first ESRCH: a gone group's id may be reused by an unrelated one
	for end := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := killTagged(tag); err != nil {
			return err
		}
		left, err := tagged(tag) // zombies never count: their env is unreadable
		if err != nil {
			return fmt.Errorf("tree sweep: %w", err)
		}
		group = group && !errors.Is(syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL), syscall.ESRCH)
		if len(left) == 0 && !group {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("tree sweep: %d tagged processes survived, group %d left: %v", len(left), cmd.Process.Pid, group)
		}
	}
}

// TreeTag is TreeCancel's tag in cmd.Env, the last one: an inherited tag comes first.
func TreeTag(cmd *exec.Cmd) (tag string) {
	for _, kv := range cmd.Env {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, TreeTagPrefix) {
			tag = k
		}
	}
	return tag
}

// TreeAlive reports whether any live process still carries tag, wherever its cwd went.
func TreeAlive(tag string) (bool, error) {
	pids, err := tagged(tag)
	return len(pids) > 0, err
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
