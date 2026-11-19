package ask

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/askspec"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func init() { askspec.Register(askSchema) }

// askSchema is ask's FlagSet as spec keys; choices come from the same values the validators check.
func askSchema() askspec.Schema {
	fs, _ := askFlagSet()
	return askspec.FromFlagSet(fs, askspec.Opts{
		Choices: map[string][]string{"fallback": router.FallbackModes, "permissions": permModes,
			"format": agent.Formats, "effort": agentid.EffortNames(), "as": {"auto", "user", "sp"}},
		JSON: []string{"overlay", "agents", "output-schema"},
	})
}

// SpecArgv reads a --spec value (file, inline JSON or -) into ask argv and the spec's prompt.
func SpecArgv(value string, stdin io.Reader) ([]string, string, error) {
	raw, err := agentconfig.ReadJSONSource("spec", value, stdin)
	if err != nil {
		return nil, "", err
	}
	s, _ := askspec.Current()
	return s.Argv(raw)
}

// withSpec puts the spec's argv first, so a typed scalar flag wins and a typed list adds to the spec's.
func withSpec(f *askFlags, args, pos []string) ([]string, error) {
	argv, prompt, err := SpecArgv(f.spec, os.Stdin)
	if err != nil {
		return nil, err
	}
	out := append(argv, dropSpec(args)...)
	if prompt != "" && len(pos) == 0 {
		if _, term := walkArgs(args); term < 0 {
			out = append(out, "--")
		}
		out = append(out, prompt)
	}
	return out, nil
}

// parseAsk parses args, re-parses them with --spec's argv first, then refuses a leading agent word.
func parseAsk(verb string, args []string) (*flag.FlagSet, *askFlags, []string, []string, int, bool) {
	fs, f := askFlagSet()
	if verb != "" {
		retargetPage(fs, verb)
	}
	pos, code, done := parseAskArgs(fs, args)
	if !done && f.spec != "" {
		fs, f, args, pos, code, done = reparseSpec(f, verb, args, pos)
	}
	if done {
		return fs, f, args, pos, code, true
	}
	if err := refuseAgentWord(fs, verb, args, pos); err != nil {
		return fs, f, args, pos, failAskCode(f.format, f.name, err, 2), true
	}
	return fs, f, args, pos, 0, false
}

// reparseSpec re-parses args with the spec's argv first on a fresh FlagSet; f.spec survives for stdinTaken.
func reparseSpec(f *askFlags, verb string, args, pos []string) (*flag.FlagSet, *askFlags, []string, []string, int, bool) {
	merged, err := withSpec(f, args, pos)
	if err != nil {
		return nil, f, args, nil, failAskCode(f.format, f.name, err, 2), true
	}
	fs, nf := askFlagSet()
	if verb != "" {
		retargetPage(fs, verb)
	}
	pos, code, done := parseAskArgs(fs, merged)
	nf.spec = f.spec
	return fs, nf, merged, pos, code, done
}

// refuseAgentWord rejects a first task word that is exactly an agent name when --agent was not given; "--" opts out.
func refuseAgentWord(fs *flag.FlagSet, verb string, args, pos []string) error {
	if verb != "" {
		return nil
	}
	if w, ok := askspec.LeadingAgentWord(args, pos, agent.RosterNames(), helptext.Explicit(fs, "agent")); ok {
		return fmt.Errorf("%q is an agent name; use --agent %s (or -- before a prompt that starts with it)", w, w)
	}
	return nil
}

// walkArgs finds each --spec word (and its value) and the real "--" terminator, skipping other flags' values.
func walkArgs(args []string) (drop map[int]bool, term int) {
	fs, _ := askFlagSet()
	drop, term = map[int]bool{}, -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return drop, i
		}
		if len(a) < 2 || a[0] != '-' {
			continue
		}
		name, _, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name == "spec" {
			drop[i] = true
			if !hasVal && i+1 < len(args) {
				i++
				drop[i] = true
			}
			continue
		}
		if takesValue(fs.Lookup(name)) && !hasVal {
			i++
		}
	}
	return drop, term
}

func takesValue(f *flag.Flag) bool {
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// dropSpec removes --spec V and --spec=V so the re-parse does not read the spec twice.
func dropSpec(args []string) []string {
	drop, _ := walkArgs(args)
	var out []string
	for i, a := range args {
		if !drop[i] {
			out = append(out, a)
		}
	}
	return out
}

// canonicalArgv is the typed flags in name order plus the task: the command a re-run types.
func canonicalArgv(fs *flag.FlagSet, prompt string, fromStdin bool) []string {
	argv := []string{"claude-code", "agents", "ask"}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "spec" || f.Name == "dry-run" {
			return
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			if f.Value.String() == "true" {
				argv = append(argv, "--"+f.Name)
			} else {
				argv = append(argv, "--"+f.Name+"=false")
			}
			return
		}
		if l, ok := f.Value.(interface{ IsRepeatable() bool }); ok && l.IsRepeatable() {
			for _, v := range textutil.SplitCSV(f.Value.String()) {
				argv = append(argv, "--"+f.Name, v)
			}
			return
		}
		argv = append(argv, "--"+f.Name, f.Value.String())
	})
	if fromStdin {
		return append(argv, "-")
	}
	return append(argv, "--", prompt)
}

// dryCombos refuses --dry-run with a run shape it cannot preview.
func dryCombos(f *askFlags) error {
	if f.dryRun && (f.live || f.batchSrc != "" || f.job != "") {
		return errors.New("--dry-run cannot be combined with --live, --batch or --job")
	}
	return nil
}

// dryCtx is ctx carrying the dry doc (overlay, argv) when --dry-run is set.
func (r *askRun) dryCtx(ctx context.Context, prompt string) context.Context {
	if !r.f.dryRun {
		return ctx
	}
	return context.WithValue(ctx, dryKey{}, dryDoc{Overlay: r.ov, Argv: textutil.ShellJoin(canonicalArgv(r.fs, prompt, taskFromStdin(r.pos)))})
}

// dryKey carries a --dry-run's dryDoc (overlay, argv) to askPrepareAndRun, which then stops before Prepare.
type dryKey struct{}

// dryDoc is what --dry-run prints.
type dryDoc struct {
	Agent       string              `json:"agent"`
	Model       string              `json:"model"`
	Route       string              `json:"route"`
	Hops        []string            `json:"hops"`
	Permissions string              `json:"permissions"`
	Overlay     agentconfig.Overlay `json:"overlay"`
	Argv        string              `json:"argv"`
	Preflight   []string            `json:"preflight,omitempty"`
}

// dryRun fills base (overlay, argv) with what the run would do; it stops before any record, claim, spawn or model call.
func dryRun(ctx context.Context, req agent.RunOptions, base dryDoc, format string, out io.Writer) int {
	o, _, ch, err := newClient().Plan(agent.PeekContext(ctx), req)
	if err != nil {
		return failAskCode(format, req.Agent, err, prepareExitCode(err))
	}
	doc := base
	doc.Agent, doc.Model, doc.Hops, doc.Permissions = o.Agent, o.Model, ch.Hops(), permLine(o.Permissions)
	if r, ok := dryRoute(ctx, o, req); ok {
		doc.Model, doc.Route = cmp.Or(o.Model, r.Model), r.Line()
	}
	if ch.Preflight != nil {
		doc.Preflight = ch.Preflight.Lines()
	}
	return doc.print(format, out)
}

// dryRoute is the first hop's route, from the calls `gateway resolve` makes; ok is false when it cannot resolve.
func dryRoute(ctx context.Context, o, req agent.RunOptions) (agent.ModelRoute, bool) {
	ctx = agent.PeekContext(ctx)
	reg, err := providers.Load()
	if err != nil {
		return agent.ModelRoute{}, false
	}
	q := agent.QueryFor(ctx, agent.ModelQuery{Agent: o.Agent, Model: req.Model, Profile: req.Provider, Account: req.Account,
		Effort: req.Effort, Fallback: o.Fallback, Task: o.Prompt, Tools: o.Tools,
		Held: req.Background || req.SessionID != "" || req.Host != ""})
	hop, note, err := agent.RunQuery(ctx, reg, q, router.RuleClassifier{}.Alias(o.Prompt))
	if err != nil {
		return agent.ModelRoute{}, false
	}
	r, _, err := agent.ExplainRun(reg, agent.QueryFor(ctx, hop), time.Now())
	if err != nil {
		return agent.ModelRoute{}, false
	}
	r.Note = strings.Join(slices.DeleteFunc([]string{note, r.Note}, func(s string) bool { return s == "" }), "; ")
	return r, true
}

func (d dryDoc) print(format string, out io.Writer) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(out, d); err != nil {
			return failAsk(format, d.Agent, err)
		}
		return 0
	}
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(out, "%-12s %s\n", k+":", v)
		}
	}
	row("agent", d.Agent)
	row("model", d.Model)
	row("route", d.Route)
	row("hops", strings.Join(d.Hops, "; "))
	row("permissions", d.Permissions)
	if !d.Overlay.Empty() {
		b, _ := json.Marshal(d.Overlay)
		row("overlay", string(b))
	}
	row("argv", d.Argv)
	for _, l := range d.Preflight {
		row("preflight", l)
	}
	return 0
}

// permLine is the mode plus each rule list, as --permissions and --allow/--ask/--deny take them.
func permLine(p agent.Permissions) string {
	parts := []string{cmp.Or(p.Mode, "default")}
	for _, l := range []struct {
		name  string
		rules []agent.Rule
	}{{"allow", p.Allow}, {"ask", p.Ask}, {"deny", p.Deny}} {
		if len(l.rules) > 0 {
			parts = append(parts, l.name+" "+strings.Join(agent.RuleStrings(l.rules), ", "))
		}
	}
	return strings.Join(parts, "; ")
}
