package claude

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// launcherEnv holds the PID that ran bin/claude; exec keeps the PID, so seeing our own means the launcher looped.
const launcherEnv = "HARNESS_CLAUDE_LAUNCHER"

// nativeSubcommands manage the install or its config and call no model, so they skip the overlay.
var nativeSubcommands = []string{"--version", "-v", "update", "upgrade", "install", "doctor", "auth", "setup-token",
	"mcp", "plugin", "plugins"}

// LaunchWrapped is bin/claude: the session-profile launch, or native claude with a stderr note if planning fails.
func LaunchWrapped(args []string) int {
	native, args := takeNative(args)
	if os.Getenv(launcherEnv) == strconv.Itoa(os.Getpid()) {
		if native == "" {
			native = newestInstalledClaude()
		}
		return execNative(native, args, errors.New("bin/claude re-entered itself"))
	}
	if len(args) > 0 && slices.Contains(nativeSubcommands, args[0]) {
		return execNative(native, args, nil)
	}
	opts, plan, err := planLaunch(native, args)
	if err != nil {
		if launchUsage(err) { // a refused --model is the user's typo: exit 2, no launch, no native fallback
			return helptext.FailUsage("%v", err)
		}
		if refuse := refuseFallback(opts, err); refuse != nil {
			deduped, changed := dedupInputs(args)
			if len(changed) == 0 {
				return helptext.Fail("%v", refuse)
			}
			if opts, plan, err = planLaunch(native, deduped); err != nil {
				return helptext.Fail("%v; a retry after deduping %s also failed: %v", refuse, strings.Join(changed, ", "), err)
			}
			fmt.Fprintf(os.Stderr, "claude: launch planned after deduping %s\n", strings.Join(changed, ", "))
		} else {
			return execNative(native, args, err)
		}
	}
	plan.Set[launcherEnv] = strconv.Itoa(os.Getpid())
	return agent.RunPlan("claude", plan, opts, nil)
}

// launchUsage is a refused launch input: a *agent.UsageError or a <profile>:<model> refusal.
func launchUsage(err error) bool {
	_, ok := errors.AsType[*agent.UsageError](err)
	return ok || errors.Is(err, providers.ErrModelSpec)
}

// planLaunch parses args and builds the session-profile plan for native.
func planLaunch(native string, args []string) (agent.LaunchOptions, agent.Plan, error) {
	as, rest, err := providers.TakeAs(args)
	var opts agent.LaunchOptions
	if err == nil {
		opts, rest, err = agent.ParseLaunchArgs(rest, true)
	}
	if err != nil {
		return opts, agent.Plan{}, err
	}
	opts.Binary = native
	plan, err := sessionPlan(as, &opts, rest)
	return opts, plan, err
}

// dedupInputs drops repeated `--settings <v>` pairs and duplicate PATH dirs; changed names what it removed.
func dedupInputs(args []string) ([]string, []string) {
	var out, changed []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--settings" && i+1 < len(args) {
			if seen[args[i+1]] {
				changed, i = append(changed, settingsLabel(args[i+1])), i+1
				continue
			}
			seen[args[i+1]] = true
		}
		out = append(out, args[i])
	}
	var dirs []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if c := filepath.Clean(d); !slices.Contains(dirs, c) {
			dirs = append(dirs, c)
		}
	}
	if joined := strings.Join(dirs, string(os.PathListSeparator)); joined != os.Getenv("PATH") {
		changed = append(changed, "PATH")
		_ = proc.Setenv("PATH", joined)
	}
	return out, changed
}

// settingsLabel names a --settings value: a file path as is, inline JSON by size only (it may hold credentials).
func settingsLabel(v string) string {
	if strings.HasPrefix(strings.TrimSpace(v), "{") {
		return fmt.Sprintf("--settings <inline JSON, %d bytes>", len(v))
	}
	return "--settings " + v
}

// takeNative strips a leading absolute path to a claude executable: the VS Code wrapper's bundled binary.
func takeNative(args []string) (string, []string) {
	if len(args) == 0 || !filepath.IsAbs(args[0]) || strings.TrimSuffix(filepath.Base(args[0]), ".exe") != "claude" {
		return "", args
	}
	if fi, err := os.Stat(args[0]); err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return "", args
	}
	return args[0], args[1:]
}

// refuseFallback is non-nil when a pinned provider route must not fall back to native claude's Anthropic login.
func refuseFallback(o agent.LaunchOptions, cause error) error {
	reg, err := providers.Load()
	if err == nil {
		var sel providers.Selection
		if sel, err = providers.Select(reg, o.Provider); err == nil && sel.Provider.Auth.Method() == providers.AuthCLI {
			return nil
		}
	}
	return fmt.Errorf("claude: harness launch failed on a pinned provider route (%v); not falling back to native claude; "+
		"run `claude-code doctor`, or `claude --profile anthropic` / unpin the profile", cause)
}

// execNative runs native claude; cause, when set, is why the overlay is skipped, with no harness settings added.
func execNative(native string, args []string, cause error) int {
	if native == "" {
		var err error
		if os.Getenv(launcherEnv) == strconv.Itoa(os.Getpid()) {
			err = errors.New("claude: launcher loop and no native install in ~/.local/share/claude/versions")
		} else {
			native, err = paths.NativeClaude()
		}
		if err != nil {
			return helptext.Fail("%v", err)
		}
	}
	if cause != nil {
		fmt.Fprintf(os.Stderr, "claude: harness launch failed (%v); running native claude\n", cause)
	}
	env := append(os.Environ(), launcherEnv+"="+strconv.Itoa(os.Getpid()))
	// exec ends this process: a refresh token already sent must reach the vault first
	providers.FinishExchanges()
	return helptext.Fail("exec %s: %v", native, proc.Exec(native, args, env))
}

// Ultrareview is the scope of one `claude ultrareview`; Post is never sent, so findings stay local.
type Ultrareview struct {
	Base, Commit, Focus string
	Uncommitted, PR     bool
	JSON                bool
}

// Argv builds the ultrareview argument list; a scope the cloud review cannot express is refused.
func (u Ultrareview) Argv() ([]string, error) {
	if u.Uncommitted || u.Commit != "" {
		return nil, errors.New("ultrareview reviews the branch, a base branch or a PR: --uncommitted and --commit are not supported")
	}
	args, err := u.targetArg()
	if err != nil {
		return nil, err
	}
	if u.JSON {
		args = append(args, "--json")
	}
	return args, nil
}

func (u Ultrareview) targetArg() ([]string, error) {
	switch {
	case u.Base != "" && u.PR:
		return nil, errors.New("at most one of --base, --pr")
	case strings.HasPrefix(u.Base, "-"):
		return nil, fmt.Errorf("--base ref %q must not start with -", u.Base)
	case u.PR:
		if _, err := strconv.ParseUint(u.Focus, 10, 32); err != nil {
			return nil, errors.New("ultrareview --pr takes the PR number: --pr 123")
		}
		return []string{"ultrareview", u.Focus}, nil
	case u.Focus != "":
		return nil, errors.New("ultrareview takes no focus words")
	case u.Base != "":
		return []string{"ultrareview", u.Base}, nil
	default:
		return []string{"ultrareview"}, nil
	}
}

// RunUltrareview runs `claude ultrareview` on Claude's own Anthropic login and returns its exit code.
func RunUltrareview(ctx context.Context, argv []string) int {
	bin, err := ClaudeBinary()
	if err != nil {
		return helptext.Fail("claude review: %v", err)
	}
	unset := []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} // proxy and profile wiring: the cloud runs on the own login
	return agent.RunOwnLogin(ctx, "claude", bin, unset, argv)
}

// hashedOverlayPath names the file by content so a later launch never rewrites one a live session loaded.
func hashedOverlayPath(path string, data []byte) string {
	base := strings.TrimSuffix(path, ".json")
	if i := strings.LastIndex(base, "-"); i >= 0 && len(base)-i-1 == 12 { // drop an earlier hash
		if _, err := strconv.ParseUint(base[i+1:], 16, 64); err == nil {
			base = base[:i]
		}
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s-%x.json", base, sum[:6])
}
