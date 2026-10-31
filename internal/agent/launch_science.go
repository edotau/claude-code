package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// scienceWipePrefixes are Claude Code / harness namespaces: a live session's IPC tokens and routing ride them.
// ANTHROPIC_ is exact-match only (wipeKeys): claude-science reads its own ANTHROPIC_* SSO/BYOK knobs.
var scienceWipePrefixes = []string{"CLAUDE_", "CLAUDECODE", "HARNESS_", "GEMINI_"}

// scienceWiped reports whether key must not reach claude-science, which signs in with its own OAuth.
func scienceWiped(key string) bool {
	if strings.HasPrefix(key, "CLAUDE_SCIENCE_") {
		return false
	}
	if slices.Contains(wipeKeys, key) {
		return true
	}
	return slices.ContainsFunc(scienceWipePrefixes, func(p string) bool { return strings.HasPrefix(key, p) })
}

// Science resolves `claude-code science`: the real claude-science binary, minus the harness env that
// would send its login to the router or gateway (HTTP 401). args pass through untouched.
func Science(env, args []string) (Plan, error) {
	bin, err := paths.LookPathReal("claude-science")
	if err != nil {
		return Plan{}, errors.New("claude-science not found on PATH; install Claude for Life Sciences from https://claude.ai/download")
	}
	plan := Plan{Binary: bin, Set: map[string]string{}, Argv: args}
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); scienceWiped(k) {
			plan.Unset = append(plan.Unset, k)
		}
	}
	for k := range providers.CustomEnv() {
		if scienceWiped(k) {
			plan.Unset = append(plan.Unset, k)
		}
	}
	return plan, nil
}

// ScienceEnv is Science over the live environment, with the site config and pinned serve ports applied.
func ScienceEnv(args []string) (Plan, error) {
	port, rest, err := scienceArgs(args, scienceConfigPort(scienceTOML))
	if err != nil {
		return Plan{}, err
	}
	cfg, err := scienceDefaultsFile()
	if err != nil {
		return Plan{}, err
	}
	argv, err := scienceArgv(rest, cfg, port, portBusy)
	if err != nil {
		return Plan{}, err
	}
	return Science(os.Environ(), argv)
}

// scienceTOML is the generic claude-science config shipped in the binary; company/HPC detail stays out of it.
//
//go:embed science.toml
var scienceTOML []byte

// scienceConfigPort reads the top-level `port = N` (a `port` inside a [table] is not the app port); 3000 if unset.
func scienceConfigPort(raw []byte) int {
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "port" {
			continue
		}
		value, _, _ = strings.Cut(value, "#")
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 && n < 65535 {
			return n
		}
	}
	return 3000
}

// scienceDefaultsFile materializes the embedded config only for a box without claude-science's own config.toml,
// so a user's existing grants are never replaced; "" means pass no --config.
func scienceDefaultsFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if paths.IsFile(filepath.Join(home, ".claude-science", "config.toml")) {
		return "", nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "claude-code", "science.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, paths.AtomicWrite(path, scienceTOML, 0o600)
}

// scienceArgs splits the harness's leading --port (either argv form) from the vendor argv, which passes untouched.
func scienceArgs(args []string, defPort int) (port int, rest []string, err error) {
	port = defPort
	for len(args) > 0 {
		name, value, inline := strings.Cut(args[0], "=")
		if name != "--port" {
			break
		}
		if !inline {
			if len(args) < 2 {
				return 0, nil, errors.New("--port requires a value")
			}
			value, args = args[1], args[1:]
		}
		args = args[1:]
		if port, err = strconv.Atoi(value); err != nil || port <= 0 || port >= 65535 {
			return 0, nil, fmt.Errorf("--port %q: want a port number below 65535 (sandbox is port+1)", value)
		}
	}
	return port, args, nil
}

// scienceArgv appends --config (every verb: it is global, and `status` type-checks it) and, for serve, the
// pinned --port/--sandbox-port pair; a busy pin is an error, never a silent move that breaks ssh -L tunnels.
func scienceArgv(args []string, cfg string, port int, busy func(int) bool) ([]string, error) {
	argv := slices.Clone(args)
	if cfg != "" && !hasArg(args, "--config") {
		argv = append(argv, "--config", cfg)
	}
	if len(args) == 0 || args[0] != "serve" || hasArg(args, "--port") || hasArg(args, "--sandbox-port") {
		return argv, nil
	}
	for _, p := range []int{port, port + 1} {
		if busy(p) {
			return nil, fmt.Errorf("port %d is already in use: `claude-science stop` an older daemon, "+
				"or move the pin with `claude-code science --port N serve`", p)
		}
	}
	return append(argv, "--port", strconv.Itoa(port), "--sandbox-port", strconv.Itoa(port+1)), nil
}

// hasArg reports a flag in either `--f v` or `--f=v` form.
func hasArg(args []string, flag string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return a == flag || strings.HasPrefix(a, flag+"=") })
}

// portBusy reports whether a loopback TCP port is taken; a failed bind is the only honest probe.
func portBusy(port int) bool {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}
