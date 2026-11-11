package codex

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"

	"github.com/edotau/claude-code/internal/paths"
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

// codexProfileHint is the usage error for `codex --profile X` when X is no harness profile but codex's own
// $CODEX_HOME/X.config.toml exists; nil otherwise.
func codexProfileHint(o agent.LaunchOptions) error {
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

// codexFileOverrides flattens a config.toml (top-level keys and [a.b] tables, one-line values) into
// codex `-c key=value` pairs; values pass through as TOML, which codex parses itself.
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
	if !ok || v == "" || strings.HasSuffix(v, "[") || strings.HasSuffix(v, "{") || strings.HasPrefix(v, `"""`) {
		return "", fmt.Errorf("-f %s:%d: want one-line `key = value`", path, n)
	}
	return table + strings.TrimSpace(k) + "=" + v, nil
}

// codexRouteDetail reports codex's own CODEX_MODEL/CODEX_REASONING_EFFORT pins — names/booleans only,
// never a credential; unset means codex's own default (config.toml, then its ChatGPT-login catalog).
func codexRouteDetail() string {
	model, source := providers.CodexModel()
	effort, _ := providers.CodexEffort()
	return fmt.Sprintf("model=%s source=%s effort=%s", cmp.Or(model, "(default)"), source, cmp.Or(effort, "default"))
}

// codexStatus runs `codex login status` on the scrubbed agent env; tests replace it so none of it reaches the real CLI.
var codexStatus = func(ctx context.Context, bin string) ([]byte, error) {
	ctx = agent.ProbeContext(ctx, "codex") // the login status reads codex's own sign-in only
	return agent.ProbeCLI(ctx, bin, agent.EnvSpec{Mode: agent.EnvAgent}, true, "login", "status")
}
