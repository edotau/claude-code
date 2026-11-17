package hooks

import (
	"cmp"
	"context"
	"errors"
	"regexp"
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
)

// openingAskWords uses the receipt grammar for a leading Run command, retaining its flags and quoted prompt.
func openingAskWords(task string) []string {
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	command, ok := strings.CutPrefix(line, "Run:")
	if !ok {
		return nil
	}
	match := askCmdRe().FindStringSubmatchIndex(command)
	if match == nil || match[0] != 0 {
		return nil
	}
	words := shellFields(command[match[2]:])
	if len(words) < 3 {
		return nil
	}
	verb := 1
	if words[1] == "--remote" {
		verb = 3
	}
	if len(words) <= verb+1 || words[verb+1] != "ask" {
		return nil
	}
	return words
}

// opensWithAsk is a brief opening with its own ask command: never rewritten, only labelled.
func opensWithAsk(task string) bool { return len(openingAskWords(task)) > 0 }

// askLine holds the opening ask command's routing flags, positional prompt and overlay.
type askLine struct {
	agent, model, prompt string
	profile, fallback    string // --profile and --fallback as typed
	local                bool   // no flag runs it off this machine or off the line's own route: preflight predictable
	tools                bool   // --tools, or --read-only, which ask folds into tools
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
	agent = cmp.Or(flagValue(words, "agent"), agent)
	l := askLine{agent: agent, model: flagValue(words, "model"),
		profile: flagValue(words, "profile"), fallback: flagValue(words, "fallback"), local: verb == 1 && !hasFlag(words, offRoute),
		tools: hasFlag(words, []string{"tools", "read-only"}) || packsToFile(agent, words)}
	l.prompt = askPrompt(words)
	value := flagValue(words, "overlay")
	if value == "" || value == "-" { // "" would read $COWORKER_OVERLAY, "-" the hook's stdin: no prediction
		return l
	}
	if cfg, err := agentconfig.LoadOverlayContext(ctx, value, strings.NewReader("")); err == nil { // unreadable: no prediction
		l.cfg = cfg
	}
	return l
}

// askPrompt follows Go flag parsing; a lone dash asks for stdin rather than supplying task text.
func askPrompt(words []string) string {
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			words = words[i+1:]
			break
		}
		if !isFlag(w) {
			words = words[i:]
			break
		}
		name, _, inline := strings.Cut(strings.TrimLeft(w, "-"), "=")
		if !inline && slices.Contains(askValueFlags, name) {
			i++
		}
		if i >= len(words)-1 {
			return ""
		}
	}
	if len(words) > 0 && !(len(words) == 1 && words[0] == "-") {
		return strings.Join(words, " ")
	}
	return ""
}

var askValueFlags = strings.Fields("agent agents fallback profile provider as model effort workdir session debug-file " +
	"attach keep-env env allow ask deny mode overlay system-prompt append-system-prompt output-schema timeout host " +
	"format dirs files max-files max-file-bytes rubric retry job batch jobs")

// packsToFile is a --dirs/--files/--diff pack the agent reads from a staged file, which turns tools on (packPrompt).
func packsToFile(agent string, words []string) bool {
	return hasFlag(words, []string{"dirs", "files", "diff"}) && agentpkg.SchemaFor(agent).CLI.PackVia == agentpkg.PackFile
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
	if err := l.refusal(reg, agent); err != nil {
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

// flagValue is the last -name/--name value before the first positional word or "--", as Go's flag package parses.
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

func hasFlag(words, names []string) bool {
	for i := 0; i < len(words) && isFlag(words[i]); i++ {
		n, _, hasVal := strings.Cut(strings.TrimPrefix(words[i][1:], "-"), "=")
		if slices.Contains(names, n) {
			return true
		}
		if !hasVal && takesValue(words, i+1) {
			i++
		}
	}
	return false
}

func isFlag(w string) bool { return w != "--" && len(w) >= 2 && w[0] == '-' }

// takesValue is words[i] being the previous flag's value: a word or "-" (stdin); a flag means that one was boolean.
func takesValue(words []string, i int) bool {
	return i < len(words) && (words[i] == "-" || !strings.HasPrefix(words[i], "-"))
}

// shellWordRe matches one shell word, shellPieceRe a quoted or escaped piece of it, shellEscRe an escape in "…".
var (
	shellWordRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?:[^\s'"\\]+|\\.?|'[^']*'?|"(?:[^"\\]|\\.)*"?)+`)
	})
	shellPieceRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\\.?|'[^']*'?|"(?:[^"\\]|\\.)*"?`) })
	shellEscRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("\\\\([\"\\\\$`])") })
)

// shellFields splits a command line into the words a POSIX shell passes: quotes group and are removed, \ escapes.
func shellFields(line string) []string {
	words := shellWordRe().FindAllString(line, -1)
	for i, w := range words {
		words[i] = shellPieceRe().ReplaceAllStringFunc(w, unquotePiece)
	}
	return words
}

func unquotePiece(p string) string {
	switch p[0] {
	case '\\':
		return p[1:]
	case '\'':
		return strings.TrimSuffix(p[1:], "'")
	}
	return shellEscRe().ReplaceAllString(strings.TrimSuffix(p[1:], `"`), "$1")
}
