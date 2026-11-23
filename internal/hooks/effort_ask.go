package hooks

import (
	"cmp"
	"context"
	"flag"
	"io"
	"slices"
	"strings"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
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
	l = askLine{agent: words[0], model: flagValue(words[2:], "model"), profile: flagValue(words[2:], "profile"),
		provider: flagValue(words[2:], "provider"), review: true}
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
	provider             string // --provider as typed: the route the label must resolve on
	held                 bool   // --live, --background, --session, --host, --job or --batch: the pool pin never picks
	tools                bool   // --tools, or --read-only, which ask folds into tools
	review               bool   // a `<agent> review` line: the Agent call's own model is the relay's, not the vendor's
	cfg                  agentconfig.Overlay
}

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
		profile: a.Value("profile"), provider: a.Value("provider"), fallback: a.Value("fallback"),
		held:  a.On("live", "background", "session", "host", "job", "batch"),
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
	"enable-memory stdin-framed live background fail-fast diff index dry-run loop status fresh recover carry")

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

// On reports whether any named flag is set to a value ask treats as on: not empty and not "false".
func (a AskWords) On(names ...string) bool {
	on := false
	a.fs.Visit(func(f *flag.Flag) {
		v := f.Value.String()
		on = on || slices.Contains(names, f.Name) && v != "" && v != "false"
	})
	return on
}

// Prompt is the task words; a lone dash asks for stdin rather than supplying task text.
func (a AskWords) Prompt() string {
	if len(a.Pos) == 0 || len(a.Pos) == 1 && a.Pos[0] == "-" {
		return ""
	}
	return strings.Join(a.Pos, " ")
}

var askValueFlags = strings.Fields("agent agents fallback profile provider as model effort workdir session debug-file " +
	"attach keep-env env allow ask deny permissions overlay system-prompt append-system-prompt output-schema timeout host " +
	"format dirs files max-files max-file-bytes rubric retry job batch jobs save spec " +
	"loop-rounds loop-planner loop-budget-tokens loop-resume waves wave gate profiles codemode")

// packsToFile is a --dirs/--files/--diff pack the agent reads from a staged file, which turns tools on (packPrompt).
func packsToFile(agent string, a AskWords) bool {
	return a.Has("dirs", "files", "diff") && agentpkg.SchemaFor(agent).CLI.PackVia == agentpkg.PackFile
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

// RunLine is the brief's `Run:` line for ask argv, each word quoted for a shell.
func RunLine(argv []string) string {
	return "Run: " + textutil.ShellJoin(append([]string{"claude-code", "agents", "ask"}, argv...))
}

// OverlayArgs is the ask argv for a profile and a plain model: one --overlay, or --profile when unknown.
func OverlayArgs(profile, model, identity, account string) []string {
	t := overlayTarget(profile, model)
	switch t.Provider {
	case "databricks":
		t.Identity = identity
	case "github":
		t.Account = account
	case "":
		if t.Profile != "" { // no registry row: ask resolves the name and refuses it if unknown
			return append([]string{"--profile", t.Profile}, overlayFlag(route.Target{Model: t.Model})...)
		}
	}
	return overlayFlag(t)
}

// overlayTarget fills the provider from the profile's registry row; a bare provider key is its pool pick.
func overlayTarget(profile, model string) route.Target {
	reg, _ := providers.Load()
	t := route.Target{Route: route.ParseLegacy(profile), Model: model}
	if t.Provider != "" {
		t.Profile = cmp.Or(t.Profile, agentpkg.AutoModel)
	} else if reg != nil && t.Profile != "" {
		if p, err := reg.Get(t.Profile); err == nil {
			t.Provider = p.Provider
		}
	}
	return t
}

func overlayFlag(t route.Target) []string {
	if t.IsZero() && t.Model == "" {
		return nil
	}
	return []string{"--overlay", t.JSON()}
}
