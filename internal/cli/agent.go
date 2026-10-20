package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agenttree"
	"github.com/edotau/claude-code/internal/crossgen"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

func cmdAsk(args []string) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	name := fs.String("agent", "claude", "runner: claude|api|codex|gemini|opencode|copilot")
	fallback := fs.String("fallback", agent.FallbackAll, "fallback: all (vendors, then claude)|vendors|none")
	native := fs.Bool("native", false, "opencode: skip the hub ladder; run on its default login")
	provider := fs.String("provider", "", "provider or hub start (default: pin, then registry)")
	model := fs.String("model", "", "slot name, model id, or provider:model")
	effort := fs.String("effort", "", "reasoning effort: low|medium|high|xhigh|max")
	workdir := fs.String("workdir", "", "working directory for the agent")
	session := fs.String("session", "", "resume this session id")
	tools := fs.Bool("tools", false, "allow tool use where the agent can gate it (default: disabled)")
	var attach attachFlag
	fs.Var(&attach, "attach", "attach a file (repeatable); task text precedes every attachment")
	autonomous := fs.Bool("autonomous", false, "drive the leg without stopping to ask, per runner")
	enableMemory := fs.Bool("enable-memory", false, "copilot only: turn on session memory")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-attempt deadline (45m default with --tools)")
	format := fs.String("format", "text", "stream text or print the JSON result envelope")
	rubric := fs.String("rubric", "", "review standard file quoted verbatim ahead of the task")
	retry := fs.String("retry", "", "replay a logged dispatch by id (or unique id prefix); see --log")
	showLog := fs.Bool("log", false, "list the 20 most recent dispatches and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := validateAskFlags(*format, *fallback); err != nil {
		return fail("ask: %v", err)
	}
	if *showLog {
		return printDispatchLog(*format)
	}
	prompt := strings.Join(fs.Args(), " ")
	if *retry != "" {
		rec, err := agent.LoadRecord(*retry)
		if err != nil {
			return fail("ask: %v", err)
		}
		// The logged request wins; only flags typed on this command line override it.
		*name, *provider, *model, *effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
		*workdir, *session, *tools, *timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
		attach, *autonomous, *enableMemory = attachFlag(rec.Request.Attach), rec.Request.Autonomous, rec.Request.EnableMemory
		_ = fs.Parse(args) // re-apply explicit flags over the record
		prompt, *rubric = rec.Request.Prompt, ""
		fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	}
	prompt, err := resolveAskPrompt(prompt)
	if err != nil {
		return fail("ask: %v", err)
	}
	if strings.TrimSpace(prompt) == "" {
		helptext.Fprint(os.Stderr, "usage: claude-code ask [flags] <prompt | ->\n")
		helptext.Defaults(fs)
		return 2
	}
	r, chain, err := resolveAskAgent(*name, *provider, *fallback, *native)
	if err != nil {
		return fail("ask: %v", err)
	}
	if *rubric != "" {
		if prompt, err = agent.RubricPrompt(*rubric, prompt); err != nil {
			return fail("ask: %v", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var stream io.Writer = os.Stdout
	if *format == "json" {
		stream = io.Discard
	}
	if *tools && !timeoutExplicit(fs) {
		*timeout = 45 * time.Minute
	}
	if *enableMemory && *name != "copilot" {
		fmt.Fprintf(os.Stderr, "ask: --enable-memory has no effect on the %s runner (copilot only)\n", *name)
	}
	req := agent.Request{Prompt: prompt, Model: *model, Provider: *provider, Effort: *effort,
		WorkDir: *workdir, SessionID: *session, Tools: *tools, Timeout: *timeout,
		Attach: []string(attach), Autonomous: *autonomous, EnableMemory: *enableMemory}
	rec := agent.NewRecord(r.Name(), req, time.Now())
	_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run
	start := time.Now()
	notify := func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) }
	res, err := agent.RunWithFallback(ctx, r, chain, req, stream, notify)
	rec.Finish(res, err, time.Since(start))
	_ = rec.Save()
	reportAskResult(*format, res, err)
	fmt.Fprintln(os.Stderr, "ask: dispatch", rec.ID, "(claude-code ask --retry <id> replays it)")
	if err != nil {
		return fail("ask: %v", err)
	}
	return 0
}

// attachFlag collects repeatable --attach paths; Set resolves each to an absolute path and fails fast
// unless it names an existing regular file.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

func (a *attachFlag) Set(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("--attach %q: not a regular file", s)
	}
	*a = append(*a, abs)
	return nil
}

// timeoutExplicit is true when --timeout appeared on this command line, so it always beats the
// tools-aware default.
func timeoutExplicit(fs *flag.FlagSet) bool {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			explicit = true
		}
	})
	return explicit
}

func validateAskFlags(format, fallback string) error {
	if format != "text" && format != "json" {
		return fmt.Errorf("--format %q unknown (text|json)", format)
	}
	if !slices.Contains(agent.FallbackModes, fallback) {
		return fmt.Errorf("--fallback %q unknown (%s)", fallback, strings.Join(agent.FallbackModes, "|"))
	}
	return nil
}

// resolveAskPrompt reads stdin when the prompt is empty or the literal "-".
func resolveAskPrompt(prompt string) (string, error) {
	if prompt != "" && prompt != "-" {
		return prompt, nil
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(raw), nil
}

// resolveAskAgent looks up the named runner and, for a hub agent, swaps in the hub leg + chain.
func resolveAskAgent(name, provider, fallback string, native bool) (agent.Runner, []agent.Runner, error) {
	r, err := agent.Lookup(name)
	if err != nil {
		return nil, nil, err
	}
	chain := agent.FallbackChain(r, fallback)
	if native || !agent.IsHubAgent(name) {
		return r, chain, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, nil, err
	}
	start, err := agent.HubStart(reg, name, provider)
	if err != nil {
		return nil, nil, err
	}
	return agent.NewHubLeg(name, start, reg), agent.HubChain(reg, name, start, fallback), nil
}

func reportAskResult(format string, res agent.Result, err error) {
	for _, d := range res.Dropped {
		fmt.Fprintf(os.Stderr, "ask: %s has no channel on the %s agent — dropped\n", d, res.Agent)
	}
	if format == "json" {
		env := struct {
			agent.Result
			Error string `json:"error,omitempty"`
		}{Result: res}
		if err != nil {
			env.Error = err.Error()
		}
		body, _ := json.Marshal(env)
		fmt.Println(string(body))
		return
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
}

const agentsUsage = `usage: claude-code agents
       claude-code agents sync [--vendor V] [--dry-run] [--check] [--no-prune]
       claude-code agents tree [--days N] [--session ID] [--running] [--all]
                              [--watch [--interval D]] [--format text|json]`

func cmdAgents(args []string) int {
	if len(args) == 0 {
		return listRunners()
	}
	switch args[0] {
	case "sync":
		return cmdAgentsSync(args[1:])
	case "tree":
		return agenttree.Run(args[1:], paths.ConfigDir(), os.Stdout, os.Stderr)
	case "-h", "--help":
		helptext.Fprint(os.Stderr, agentsUsage+"\n")
		return 2
	}
	fail("agents: unknown subcommand %q", args[0])
	helptext.Fprint(os.Stderr, agentsUsage+"\n")
	return 1
}

// cmdAgentsSync renders agents/ into state/projections/<vendor>/agents plus each vendor's global instruction file, installs the
// copies into the vendor homes, and links skills/ into ~/.agents/skills; --vendor narrows the run to one target.
func cmdAgentsSync(args []string) int {
	fs := flag.NewFlagSet("agents sync", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	vendor := fs.String("vendor", "", "one target: "+strings.Join(crossgen.VendorNames(), "|")+" (default: all)")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	check := fs.Bool("check", false, "dry-run; exit 1 when the projection is stale")
	noPrune := fs.Bool("no-prune", false, "keep orphaned projections")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		helptext.Fprint(os.Stderr, agentsUsage+"\n")
		return 2
	}
	return crossgen.Sync(crossgen.SyncOptions{DryRun: *dryRun, Check: *check, NoPrune: *noPrune, Vendor: *vendor, Out: os.Stdout})
}

func listRunners() int {
	for _, r := range agent.Runners() {
		mark, reason := "✓", ""
		if err := r.Available(); err != nil {
			mark, reason = "✗", err.Error()
		}
		c := r.Caps()
		var caps []string
		for _, f := range []struct {
			ok   bool
			name string
		}{{c.Model, "model"}, {c.Provider, "provider"}, {c.Effort, "effort"}, {c.WorkDir, "workdir"}, {c.SessionID, "session"}, {c.Tools, "tools"}} {
			if f.ok {
				caps = append(caps, f.name)
			}
		}
		fmt.Printf("%s %-9s %-44s %s\n", mark, r.Name(), strings.Join(caps, ","), reason)
	}
	return 0
}

// printDispatchLog lists recent dispatches; the prompt preview is scrubbed of credential shapes.
func printDispatchLog(format string) int {
	recs := agent.RecentRecords(20)
	if format == "json" {
		_ = outfmt.Write(os.Stdout, recs)
		return 0
	}
	if len(recs) == 0 {
		fmt.Println("no dispatches logged yet")
		return 0
	}
	for _, r := range recs {
		preview := strings.Join(strings.Fields(r.Request.Prompt), " ")
		for w := range strings.FieldsSeq(preview) {
			if shapes.Looks(w) {
				preview = strings.ReplaceAll(preview, w, "REDACTED")
			}
		}
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		tools := ""
		if r.Request.Tools {
			tools = " tools"
		}
		agentCol := r.Agent
		if r.Fallback != "" {
			agentCol = r.Fallback + "→" + cmp.Or(r.RanAs, "?")
		}
		fmt.Printf("%-28s %-7s %-8s %-9s%s  %s\n", r.ID, r.Status, agentCol, r.Duration, tools, preview)
		if r.FallbackReason != "" {
			fmt.Printf("%-28s   fallback: %s\n", "", r.FallbackReason)
		}
	}
	return 0
}
