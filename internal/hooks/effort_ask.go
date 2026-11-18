package hooks

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// openingAskWords uses the receipt grammar for a leading Run command, retaining its flags and quoted prompt.
func openingAskWords(task string) []string {
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	command, ok := strings.CutPrefix(line, "Run:")
	if f := strings.Fields(command); !ok || len(f) > 0 && strings.Trim(f[0], "({") == "" {
		return nil // a lone group opener never opened an ask: matching one now would make new relays and refusals
	}
	if words := unwrapRelay(shell.CommandWords(command)[0]); askVerb(words) == "ask" {
		return words
	}
	return nil
}

// reviewLine is a leading `Run: claude-code <agent> review …` line's --model/--profile; ok only when it types one.
func reviewLine(task string) (l askLine, ok bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	command, found := strings.CutPrefix(line, "Run:")
	if !found {
		return askLine{}, false
	}
	segs := shell.Segments(command)
	if len(segs) != 1 {
		return askLine{}, false
	}
	words := agentCommand(relayWords(segs[0]))
	if len(words) < 2 || !externalTypes[words[0]] || words[1] != "review" {
		return askLine{}, false
	}
	l = askLine{agent: words[0], model: flagValue(words[2:], "model"), profile: flagValue(words[2:], "profile"), review: true}
	return l, l.model != "" || l.profile != ""
}

// labelLine is the opening ask line, else a review line that types a route: the one the run's label follows.
func (r *dispatchRun) labelLine(ctx context.Context, st, task string) askLine {
	if l, ok := reviewLine(task); ok && !inContextTypes[st] { // an in-context leaf never runs the line
		return l
	}
	return r.opening(ctx, task)
}

// opensWithAsk is a brief opening with its own ask command: never rewritten, only labelled.
func opensWithAsk(task string) bool { return len(openingAskWords(task)) > 0 }

// askLine holds the opening ask command's routing flags, positional prompt and overlay.
type askLine struct {
	agent, model, prompt string
	profile, fallback    string // --profile and --fallback as typed
	local                bool   // no flag runs it off this machine or off the line's own route: preflight predictable
	tools                bool   // --tools, or --read-only, which ask folds into tools
	review               bool   // a `<agent> review` line: the Agent call's own model is the relay's, not the vendor's
	cfg                  agentconfig.Overlay
}

// offRoute are ask flags taking the run to another host, lead, record or batch: the hook predicts no preflight.
var offRoute = []string{"agents", "native", "provider", "host", "retry", "resume", "live", "job", "jobs", "batch"}

func (r *dispatchRun) opening(ctx context.Context, task string) askLine {
	if r.line == nil || r.lineTask != task {
		l := parseAskLine(ctx, task)
		r.line, r.lineTask = &l, task
	}
	return *r.line
}

// parseAskLine reads the flags a shell would see on the opening ask command; text inside a quoted task is no flag.
func parseAskLine(ctx context.Context, task string) askLine {
	words := openingAskWords(task)
	if len(words) == 0 {
		return askLine{}
	}
	verb := 1
	if words[1] == "--remote" {
		verb = 3
	}
	agent := strings.TrimPrefix(words[verb], "agents")
	words = words[verb+2:]
	a, ok := ParseAskWords(words)
	if !ok { // ask exits 2 on this line: nothing to predict
		return askLine{agent: agent}
	}
	agent = cmp.Or(a.Value("agent"), agent)
	l := askLine{agent: agent, model: a.Value("model"),
		profile: a.Value("profile"), fallback: a.Value("fallback"), local: verb == 1 && !a.Has(offRoute...),
		tools: a.Has("tools", "read-only") || packsToFile(agent, a)}
	l.prompt = a.Prompt()
	value := a.Value("overlay")
	if value == "" || value == "-" { // "" would read $COWORKER_OVERLAY, "-" the hook's stdin: no prediction
		return l
	}
	if cfg, err := agentconfig.LoadOverlayContext(ctx, value, strings.NewReader("")); err == nil { // unreadable: no prediction
		l.cfg = cfg
	}
	return l
}

// AskWords is an ask command line parsed by helptext.ParseArgs, the parser `agents ask` itself runs.
type AskWords struct {
	fs  *flag.FlagSet
	Pos []string // the task words
}

// askBoolFlags are ask's boolean flags; every other registered flag takes a value (a test pins both lists to ask's).
var askBoolFlags = strings.Fields("native tools read-only no-tools no-persist safe-mode direct autonomous fleet " +
	"enable-memory stdin-framed live background fail-fast diff index")

// ParseAskWords parses words as ask does (flags anywhere, none after "--"); ok is false where ask would exit 2.
func ParseAskWords(words []string) (AskWords, bool) {
	fs := helptext.NewFlagSet("ask", helptext.Page{})
	fs.SetOutput(io.Discard)
	for _, n := range askBoolFlags {
		fs.Bool(n, false, "")
	}
	for _, n := range askValueFlags {
		fs.String(n, "", "")
	}
	pos, err := helptext.ParseArgs(fs, words)
	return AskWords{fs, pos}, err == nil
}

// Value is the last typed value of flag name, "" when untyped.
func (a AskWords) Value(name string) string {
	v := ""
	a.fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			v = f.Value.String()
		}
	})
	return v
}

// Has reports whether any of names was typed.
func (a AskWords) Has(names ...string) bool {
	found := false
	a.fs.Visit(func(f *flag.Flag) { found = found || slices.Contains(names, f.Name) })
	return found
}

// Prompt is the task words; a lone dash asks for stdin rather than supplying task text.
func (a AskWords) Prompt() string {
	if len(a.Pos) == 0 || len(a.Pos) == 1 && a.Pos[0] == "-" {
		return ""
	}
	return strings.Join(a.Pos, " ")
}

var askValueFlags = strings.Fields("agent agents fallback profile provider as model effort workdir session debug-file " +
	"attach keep-env env allow ask deny mode overlay system-prompt append-system-prompt output-schema timeout host " +
	"format dirs files max-files max-file-bytes rubric retry job batch jobs save")

// packsToFile is a --dirs/--files/--diff pack the agent reads from a staged file, which turns tools on (packPrompt).
func packsToFile(agent string, a AskWords) bool {
	return a.Has("dirs", "files", "diff") && agentpkg.SchemaFor(agent).CLI.PackVia == agentpkg.PackFile
}

// preflightBlock denies a Run: line `agents ask` refuses on local cached state alone; every doubt lets it through.
func preflightBlock(ctx context.Context, run *dispatchRun, task string) *core.Result {
	l := run.opening(ctx, task)
	agent := cmp.Or(l.agent, agentid.Claude)
	if !l.local || l.cfg.Host != "" || l.cfg.Remote != "" || !agentid.Known(agent) {
		return nil
	}
	if provider, _, err := router.CoworkerContext(ctx); provider != "" || err != nil {
		return nil // nested in a confinement: ask's own leave rules decide
	}
	reg, err := run.registry()
	if err != nil {
		return nil
	}
	if err := cmp.Or(agentconfig.CheckTypedProfile(reg, agent, l.profile), l.refusal(reg, agent)); err != nil {
		return preflightDeny(agent, err)
	}
	if _, _, err := labelRoute(ctx, run, map[string]any{}, "", task); errors.Is(err, agentpkg.ErrModelRoute) {
		return preflightDeny(agent, err)
	}
	return nil
}

// preflightDeny is ask's own refusal plus the fix: code cannot pick the workspace or model the brief meant.
func preflightDeny(agent string, err error) *core.Result {
	reason := "dispatch-effort: " + err.Error() + ". Fix the brief's `Run:` line " +
		"(a --profile the registry has, a --model that workspace serves, or --fallback agents) and resend; " +
		"`claude-code gateway resolve " + agent + " --profile <p> --model <m>` checks a route offline."
	var pe *agentpkg.PreflightError
	if errors.As(err, &pe) && len(pe.Finding.Served) > 0 {
		reason += " " + pe.Finding.Profile + " serves " + strings.Join(pe.Finding.Served, ", ") + "."
	}
	return &core.Result{Decision: core.Block, Reason: reason}
}

// refusal is ask's error for l's Databricks row and model, judged on reg and the row's fresh listing; nil passes.
func (l askLine) refusal(reg *providers.Registry, agent string) error {
	none := sync.OnceValue(func() bool {
		f := cmp.Or(l.fallback, l.cfg.Fallback)
		return f == router.FallbackNone || f == "" && router.DefaultFallback() == router.FallbackNone
	})
	p, model, err := l.target(reg, none)
	if err != nil || p == nil || p.Preset != providers.PresetDatabricks || model == "" {
		return err
	}
	if f := agentpkg.FreshModelFinding(agent, p, model, time.Now()); f != nil && none() {
		return &agentpkg.PreflightError{Finding: *f}
	}
	return nil
}

// target is the row and bare model l's ask preflights; err is ask's refusal of the row, a nil row no prediction.
func (l askLine) target(reg *providers.Registry, none func() bool) (*providers.Provider, string, error) {
	model, typed := l.typedModel(reg)
	if l.cfg.Provider == router.RouteDatabricks {
		return l.overlayTarget(reg, typed, model)
	}
	name := cmp.Or(l.profile, typed)
	if l.cfg.Provider != "" || name == "" || name == agentpkg.LeadSession || (typed != "" && name != typed) {
		return nil, "", nil
	}
	p, err := reg.Get(name)
	if err != nil && !none() {
		return nil, "", nil // a --profile lead with a fallback: ask routes on, it does not refuse
	}
	return p, model, err
}

// typedModel is the line's model with a leading `<row>:` cut off, and that row ("" when it names no row of reg).
func (l askLine) typedModel(reg *providers.Registry) (model, typed string) {
	model = cmp.Or(l.model, l.cfg.Model)
	if prof, bare, ok := strings.Cut(model, ":"); ok && reg.Providers[prof] != nil {
		return bare, prof
	}
	return model, ""
}

// overlayTarget is target under overlay databricks:<p>; auto, bare, or a flag naming another row is not predicted.
func (l askLine) overlayTarget(reg *providers.Registry, typed, model string) (*providers.Provider, string, error) {
	p := l.cfg.Profile
	if p == "" || p == router.AutoArm || cmp.Or(l.profile, p) != p || cmp.Or(typed, p) != p {
		return nil, "", nil
	}
	if err := reg.ServingRow(p); err != nil {
		return nil, "", err
	}
	return reg.Providers[p], model, nil
}

// workAgent is the external agent the line's --agent asks for ("" = Claude); with no --agent, the leaf type's.
func (l askLine) workAgent(st, task string) string {
	switch {
	case l.agent == "":
		return workAgent(st, task)
	case externalTypes[l.agent]:
		return l.agent
	}
	return ""
}

// flagValue is a `<agent> review` flag: its parser stops at the first positional word, which ask's does not.
func flagValue(words []string, name string) string {
	v := ""
	for i := 0; i < len(words) && isFlag(words[i]); i++ {
		n, val, hasVal := strings.Cut(strings.TrimPrefix(words[i][1:], "-"), "=")
		if !hasVal && takesValue(words, i+1) {
			val, hasVal, i = words[i+1], true, i+1
		}
		if n == name && hasVal {
			v = val
		}
	}
	return v
}

func isFlag(w string) bool { return w != "--" && len(w) >= 2 && w[0] == '-' }

// takesValue is words[i] being the previous flag's value: a word or "-" (stdin); a flag means that one was boolean.
func takesValue(words []string, i int) bool {
	return i < len(words) && (words[i] == "-" || !strings.HasPrefix(words[i], "-"))
}
