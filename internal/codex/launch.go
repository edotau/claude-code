package codex

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// codexImageArgs adds `-i <img>` after the exec subcommand; -i is variadic, so the next token must be a flag.
func codexImageArgs(argv, attach []string) []string {
	images, _ := agent.SplitImages(attach)
	if len(images) == 0 || len(argv) == 0 || argv[0] != "exec" {
		return argv
	}
	out := []string{"exec"}
	for _, p := range images {
		out = append(out, "-i", p)
	}
	return append(out, argv[1:]...)
}

// sessionFromBanner reads codex exec's `session id: <id>` line, so a new run's envelope can be resumed.
func sessionFromBanner(banner string) string {
	for line := range strings.SplitSeq(banner, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "session id:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ProfileHint is the usage error for `codex --profile X` if X is no harness profile but X.config.toml exists.
func (codexEngine) ProfileHint(o agent.LaunchOptions) error {
	if o.Provider == "" || strings.ContainsAny(o.Provider, `/\`) {
		return nil
	}
	home, err := paths.CodexHome()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(home, o.Provider+".config.toml")); err != nil {
		return nil
	}
	return &agent.UsageError{Msg: fmt.Sprintf("claude-code codex: --profile %q is not a harness profile; "+
		"for codex's own config profile ($CODEX_HOME/%s.config.toml) use -p %s", o.Provider, o.Provider, o.Provider)}
}

// codexFileOverrides flattens a config.toml (top-level keys, [a.b] tables, one-line values) into `-c key=value` pairs.
func codexFileOverrides(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	defer f.Close()
	var out []string
	table := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[["):
			return nil, fmt.Errorf("-f %s:%d: arrays of tables are not supported", path, n)
		case agentconfig.TOMLTableName(line) != "":
			table = agentconfig.TOMLTableName(line) + "."
		default:
			value, err := codexOverrideValue(line, table, path, n)
			if err != nil {
				return nil, err
			}
			out = append(out, "-c", value)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	return out, nil
}

func codexOverrideValue(line, table, path string, n int) (string, error) {
	k, v, ok := strings.Cut(line, "=")
	v = strings.TrimSpace(v)
	if !ok || v == "" || strings.HasSuffix(v, "[") || strings.HasSuffix(v, "{") || strings.HasSuffix(v, ",") ||
		strings.HasPrefix(v, `"""`) || strings.HasPrefix(v, "'''") {
		return "", fmt.Errorf("-f %s:%d: want one-line `key = value`", path, n)
	}
	return table + strings.TrimSpace(k) + "=" + v, nil
}

// RouteDetail reports CODEX_MODEL/CODEX_EFFORT pins without credentials; unset uses codex defaults.
func (codexEngine) RouteDetail(context.Context) (string, bool) {
	model, source := providers.CodexModel()
	effort, _ := providers.CodexEffort()
	return fmt.Sprintf("model=%s source=%s effort=%s", cmp.Or(model, "(default)"), source, cmp.Or(effort, "default")), true
}

// codexSandbox is read-only without tools, else the record's sandboxMode or workspace-write.
func codexSandbox(tools bool) (string, error) {
	if !tools {
		return "read-only", nil
	}
	rec, err := agentconfig.Settings(agentid.Codex)
	if err != nil {
		return "read-only", fmt.Errorf("codex: agent settings: %w", err)
	}
	switch m := rec.Extras[extraSandboxMode]; m {
	case "", "workspace-write", "danger-full-access":
		return cmp.Or(m, "workspace-write"), nil
	default:
		return "read-only", fmt.Errorf("codex: agent settings: sandboxMode %q is not a sandbox; accepted values: "+
			"workspace-write, danger-full-access", m)
	}
}

// codexNetwork reports the overlay's codexNetwork under a workspace-write tools run; a nested dispatch may not set it.
func codexNetwork(ctx context.Context, tools bool, sandbox string) (bool, error) {
	if !tools || sandbox != "workspace-write" {
		return false, nil
	}
	if o, err := agentconfig.LoadOverlayContext(ctx, "", nil); err != nil || !o.CodexNetwork {
		return false, nil // a bad overlay is refused where it is loaded; sealed is the strict reading
	}
	if proc.InDispatch(ctx) { // an outer dispatch's sandbox decides; only a top-level ask may open network
		return false, &agent.UsageError{Msg: "codex: overlay codexNetwork is refused inside a dispatch; set it on the outermost ask"}
	}
	return true, nil
}

// codexCaches are the Go cache variables a workspace-write run needs writable, with their dirs under the temp dir.
var codexCaches = [][2]string{{"GOCACHE", "go-build"}, {"GOLANGCI_LINT_CACHE", "lint"}, {"GOMODCACHE", "gomod"}}

// codexToolsEnv adds the network note and, under workspace-write, temp-dir Go caches the caller left unset.
func codexToolsEnv(ctx context.Context, env []string, tools bool, sandbox string) (out, notes []string, remove func(), err error) {
	remove = func() {}
	if !tools {
		return env, nil, remove, nil
	}
	if sandbox != "workspace-write" {
		return env, notes, remove, nil
	}
	network, err := codexNetwork(ctx, tools, sandbox)
	if err != nil {
		return env, notes, remove, err
	}
	if network {
		notes = append(notes, "codex: sandbox network open, all egress (overlay codexNetwork)")
	}
	dir, err := os.MkdirTemp("", "codex-go-")
	if err != nil {
		return env, notes, remove, fmt.Errorf("codex: go cache dir: %w", err)
	}
	values := proc.ValuesFromEnv(env)
	run, ok := proc.ValuesFromContext(ctx)
	if !ok {
		run = proc.ValuesFromEnv(os.Environ())
	}
	for _, c := range codexCaches {
		_, inProcess := run[c[0]]
		if _, inEnv := values[c[0]]; !inProcess && !inEnv {
			values = values.With(c[0], filepath.Join(dir, c[1]))
		}
	}
	return values.Env(), notes, func() { _ = os.RemoveAll(dir) }, nil
}

// codexCacheArgv repeats env's Go cache vars as shell-policy overrides; a user config's `inherit = "core"` drops them.
func codexCacheArgv(argv, env []string) []string {
	at := len(argv)
	if i := slices.Index(argv, "--skip-git-repo-check"); i >= 0 {
		at = i
	}
	values := proc.ValuesFromEnv(env)
	var add []string
	for _, c := range codexCaches {
		if v := values[c[0]]; v != "" {
			add = append(add, "-c", "shell_environment_policy.set."+c[0]+"="+agentconfig.TOMLQuote(v))
		}
	}
	return slices.Insert(slices.Clone(argv), at, add...)
}

// codexStatus runs `codex login status` on the scrubbed agent env; tests replace it so none of it reaches the real CLI.
var codexStatus = func(ctx context.Context, bin string) ([]byte, error) {
	ctx = agent.ProbeContext(ctx, "codex") // the login status reads codex's own sign-in only
	return agent.ProbeCLI(ctx, bin, agent.EnvSpec{Mode: agent.EnvAgent}, true, "login", "status")
}

var (
	codexJSONMu    sync.Mutex
	codexJSONKnown bool
	codexJSONValue bool
)

// codexHasJSON is true when the installed codex has `exec --json`; only a definitive probe is cached per process.
func codexHasJSON() bool {
	codexJSONMu.Lock()
	defer codexJSONMu.Unlock()
	if codexJSONKnown {
		return codexJSONValue
	}
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return false
	}
	line := tools.ProbedSaltedLine(bin, "exec-json", probeExecJSON)
	if line != "" {
		codexJSONKnown, codexJSONValue = true, line == "true"
	}
	return line == "true"
}

// probeExecJSON is "true" or "false" when `exec --help` answered definitively, else "" (a timeout is never cached).
func probeExecJSON(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "exec", "--help")
	cmd.WaitDelay = time.Second                                                        // a helper holding the pipe must not outlive the timeout
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent}.Environ(agent.ProbeContext(ctx, "")) // no login or route credential
	out, runErr := cmd.CombinedOutput()
	has := strings.Contains(string(out), "--json")
	if ctx.Err() != nil || runErr != nil && !has {
		return ""
	}
	if has {
		return "true"
	}
	return "false"
}
