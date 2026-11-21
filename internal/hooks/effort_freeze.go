package hooks

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"
)

// freezeAuto rewrites an auto ask Run: line to the model its label would peek, as `agents label --run` does.
func (e *effortPlan) freezeAuto(ctx context.Context, task string) (out, note string) {
	words := openingAskWords(task)
	st, _ := e.updated["subagent_type"].(string)
	line := e.run.labelLine(ctx, st, task)
	if len(words) < 3 || !freezable(words, line) {
		return task, ""
	}
	r, ok, auto, err := labelRoute(ctx, e.run, e.updated, st, task)
	if err != nil || !ok || !auto || r.Model == "" {
		return task, ""
	}
	profile, id := r.Profile, r.Model
	if p, m, _ := agentpkg.RotationFreeze(typedProfile(ctx, line.profile), cmp.Or(line.workAgent(st, task), agentid.Claude), task, line.model); m != "" {
		profile, id = p, m // stamps the rotation, as the run's own pick would
	}
	out = withOverlay(task, words, OverlayArgs(profile, id, "", ""))
	if out == task {
		return task, ""
	}
	return out, fmt.Sprintf("dispatch-effort: froze the auto pick to %s in the Run: line (the run no longer re-picks).", id)
}

// freezable is an ask line with no overlay, spec or typed provider, which a frozen --overlay would collide with.
func freezable(words []string, line askLine) bool {
	return !line.review && line.provider == "" && !slices.ContainsFunc(words, func(w string) bool {
		return strings.HasPrefix(w, "--overlay") || strings.HasPrefix(w, "--spec")
	})
}

// typedProfile carries the line's --profile as the context overlay RotationFreeze reads, so the pick stays on it.
func typedProfile(ctx context.Context, profile string) context.Context {
	args := OverlayArgs(profile, "", "", "")
	if profile == "" || len(args) != 2 || args[0] != "--overlay" {
		return ctx
	}
	_, values := agentpkg.RunValues(ctx, nil)
	return proc.WithValues(ctx, values.With(route.OverlayVar, args[1]))
}

// withOverlay swaps the line's --model and --profile for overlay, put right after the ask verb; task as is if not plain.
func withOverlay(task string, words, overlay []string) string {
	verb := 1
	if words[1] == "--remote" {
		verb = 3
	}
	if len(words) < verb+2 {
		return task
	}
	head, args := words[:verb+2], words[verb+2:]
	kept := dropFlags(args, "--model", "--profile")
	lead := len(task) - len(strings.TrimLeft(task, " \t\r\n"))
	line, rest, hasRest := strings.Cut(task[lead:], "\n")
	if openQuote(line) || !plainCommand(strings.TrimPrefix(line, "Run:"), words) {
		return task
	}
	out := task[:lead] + "Run: " + textutil.ShellJoin(slices.Concat(head, overlay, kept))
	if hasRest {
		out += "\n" + rest
	}
	return out
}

// dropFlags is args without the named flags, each in its `--f v` and `--f=v` spelling.
func dropFlags(args []string, names ...string) []string {
	var kept []string
	for i := 0; i < len(args); i++ {
		switch {
		case slices.Contains(names, args[i]):
			i++
		case slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(args[i], n+"=") }):
		default:
			kept = append(kept, args[i])
		}
	}
	return kept
}
