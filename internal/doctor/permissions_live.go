package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// argvLauncher stands in for claude's launcher: it keeps the arguments the launch hands it and writes no plan.
type argvLauncher struct{ args *[]string }

func (l argvLauncher) Claude(_ context.Context, _ agent.ClaudeOptions, args []string) (agent.ClaudeLaunch, error) {
	*l.args = slices.Clone(args)
	return agent.ClaudeLaunch{Binary: "claude", Argv: args}, nil
}

// liveLaunch is the `ask --live` session of name. A live holder hands its record's request to the backend as it is,
// so the policy it runs under is the one the backend resolves, not the ask path's checkRun layering.
func liveLaunch(readOnly bool) func(string, string) pathLaunch {
	return func(name, wd string) pathLaunch {
		row, _ := agent.LookupSchema(name)
		switch {
		case !row.HasLive():
			return pathLaunch{skip: name + " has no live mode"}
		case name == "claude":
			return claudeLive(readOnly, wd)
		}
		return resolvedLive(name, readOnly, wd)
	}
}

// resolvedLive is a live session whose backend renders the settings ResolveSettings resolves for the request (agy).
func resolvedLive(name string, readOnly bool, wd string) pathLaunch {
	req := agent.RunOptions{Agent: name, Tools: true, ReadOnly: readOnly, WorkDir: wd}
	st, err := agent.ResolveSettings(name, req, providers.Target{}, true)
	if err != nil {
		return pathLaunch{fail: "live settings: " + err.Error()}
	}
	l := liveMode(name, readOnly)
	l.judge = withSafety(name, wd, permLayer(name, st.Permissions, readOnly, wd))
	l.via = "ResolveSettings(tools" + map[bool]string{true: ", read-only"}[readOnly] + "): permissions " + permSummary(st.Permissions)
	return l
}

// liveMode is the column a live session of the request runs in.
func liveMode(name string, readOnly bool) pathLaunch {
	flags := []string{"--tools"}
	if readOnly {
		flags = append(flags, "--read-only")
	}
	return argsLaunch(name, flags, nil)
}

// permSummary is p's rule counts: enough to see a layer missing, never a rule's text.
func permSummary(p agent.Permissions) string {
	return fmt.Sprintf("%d allow, %d ask, %d deny", len(p.Allow), len(p.Ask), len(p.Deny))
}

// claudeLive builds claude's live child with a stub launcher and judges the tool set and gate policy it would run under.
func claudeLive(readOnly bool, wd string) pathLaunch {
	lc, ok := agent.SchemaFor("claude").Live()
	if !ok {
		return pathLaunch{skip: "claude has no live mode"}
	}
	var args []string
	req := agent.RunOptions{Agent: "claude", Tools: true, ReadOnly: readOnly, WorkDir: wd, Launcher: argvLauncher{&args}}
	cmd, _, _, err := lc.NewLive().Build(context.Background(), req)
	if err != nil {
		return pathLaunch{fail: "live Build: " + err.Error()}
	}
	l := liveMode("claude", readOnly)
	l.via = strings.Join(args, " ")
	pol, ok := gatePolicyEnv(cmd.Env)
	if !ok {
		l.why = "no gate on this route: Anthropic's auto-mode judge decides"
		return l
	}
	tools := ""
	if i := slices.Index(args, "--tools"); i >= 0 && i+1 < len(args) {
		tools = args[i+1]
	}
	l.judge = withSafety("claude", wd, toolScope(tools, gateDecide(pol)))
	return l
}

// gatePolicyEnv is the permission policy a child's env carries for the permission-gate.
func gatePolicyEnv(env []string) (agent.Permissions, bool) {
	var p agent.Permissions
	for _, e := range env {
		if raw, ok := strings.CutPrefix(e, agent.DispatchPermissionsEnv+"="); ok {
			return p, json.Unmarshal([]byte(raw), &p) == nil
		}
	}
	return p, false
}

// toolScope denies a call whose tool the launch's --tools list never grants; next judges the rest.
func toolScope(granted string, next permVerdict) permVerdict {
	have := strings.Split(granted, ",")
	return func(tool, arg string) permResult {
		if !slices.Contains(have, tool) {
			return permResult{"deny", "tool not in --tools", ""}
		}
		return next(tool, arg)
	}
}

// relayCommand matches every backticked ask command a wrapper doc gives for its own agent.
func relayCommand(name string) *regexp.Regexp {
	n := regexp.QuoteMeta(name)
	return regexp.MustCompile("`(claude-code (?:" + n + " ask|agents ask --agent " + n + `)\b[^` + "`" + `]*)` + "`")
}

// relayLaunch is the widest `ask` command the wrapper agents/<name>.md tells its relay to run.
func relayLaunch(name, _ string) pathLaunch {
	return relayIn(paths.ConfigDir(), name)
}

// relayIn reads dir/agents/<name>.md: [optional] flags count, <placeholders> and quoted tasks do not. A wrapper lists
// several commands (vet, gate, rescue); its ceiling is the widest, so that one is judged.
func relayIn(dir, name string) pathLaunch {
	file := filepath.Join(dir, "agents", name+".md")
	raw, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return pathLaunch{skip: "no wrapper agents/" + name + ".md in this config"}
	}
	if err != nil {
		return pathLaunch{fail: err.Error()}
	}
	var widest pathLaunch
	for _, m := range relayCommand(name).FindAllSubmatch(raw, -1) {
		var args []string
		for f := range strings.FieldsSeq(string(m[1])) {
			if f = strings.Trim(f, "[]"); f != "" && !strings.ContainsAny(f, `<"`) {
				args = append(args, f)
			}
		}
		if l := argsLaunch(name, args, nil); widest.mode == "" || scopeRank(l.mode) > scopeRank(widest.mode) {
			widest = l
		}
	}
	if widest.mode == "" {
		return pathLaunch{fail: fmt.Sprintf("%s names no `claude-code ... ask` command for %s", file, name)}
	}
	return widest
}

// scopeRank orders the modes by what they may do: no tools, read tools, then edit and run.
func scopeRank(mode string) int {
	return map[int]int{0: 0, modeRO: 1, modeTools: 2}[modeBit(mode)]
}
