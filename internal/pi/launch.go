package pi

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
)

// writeGuard is WriteGuard; tests swap it to prove a failed write refuses the launch.
var writeGuard = WriteGuard

// interactiveArgv is the dispatch lockdown minus --no-context-files (D2); every flag that stops project code loading stays.
func interactiveArgv() []string {
	return slices.DeleteFunc(slices.Clone(lockdownArgv), func(a string) bool { return a == "--no-context-files" })
}

// interactiveWire builds `claude-code pi`: Pi's TUI on the same guard as a dispatch; the launch execs Pi, so no nonce is read.
func interactiveWire(ctx context.Context, r agent.WireRoute) (env, argv []string, err error) {
	dir, model, pkgDirs, remove, err := runSetup(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	// fail closed: no guard written, no launch (builtin tools are off, so a guard that fails to load leaves Pi no tools)
	env, err = withGuard([]string{agentDirEnv + "=" + dir, keyEnv + "=" + r.Cred}, dir, true)
	if err != nil {
		remove()
		return nil, nil, err
	}
	tools := strings.Join(guardTools(agent.RunOptions{ReadOnly: agent.IsReadOnly(ctx)}), ",")
	env = append(env, guardToolsEnv+"="+tools)
	g, _ := guardFromEnv(env)
	if agent.IsDryRun(ctx) {
		remove() // a preview execs nothing, so its run dir would never be reaped
		fmt.Fprintf(agent.NoticeOut, "claude-code: pi guard preview %s tools=%s (builtin tools off)\n", g.Path, tools)
	} else {
		fmt.Fprintf(agent.NoticeOut, "claude-code: pi guard %s tools=%s (builtin tools off)\n", g.Path, tools)
	}
	argv = append(interactiveArgv(), "--model", model, "--session-dir", filepath.Join(paths.StateDir(), "pi", "sessions"))
	// the guard's -e stays first: Pi keeps the first registration of a tool name, so a package never shadows a guarded tool
	argv = append(append(argv, g.Argv()...), "--no-builtin-tools")
	return env, append(argv, PackageArgv(loadedAt(pkgDirs))...), nil
}

// LaunchArgs drops --read-only: the launcher saw it already and Wire registers only the read tools.
func (e piEngine) LaunchArgs(file string, args []string) ([]string, error) {
	args, err := e.BaseEngine.LaunchArgs(file, args)
	return slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--read-only" }), err
}
