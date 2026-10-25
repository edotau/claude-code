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
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vendoragent"
)

// askPage is `ask -h`; the task is the trailing args, or stdin when absent or "-".
var askPage = helptext.Page{
	Usage: "claude-code ask [flags] <task | ->",
	About: "Run one task headlessly on an agent runner and stream its answer; a vendor that " +
		"fails falls back per --fallback. With no task, or \"-\", the task is read from stdin.",
	Hint: "Every dispatch is logged: --log lists them, --retry <id> replays one.",
}

func cmdAsk(args []string) int {
	fs := helptext.NewFlagSet("ask", askPage)
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
	var keepEnv keepEnvFlag
	fs.Var(&keepEnv, "keep-env", "keep NAME[,NAME] from the credential scrub (repeatable)")
	autonomous := fs.Bool("autonomous", false, "drive the leg without stopping to ask, per runner")
	enableMemory := fs.Bool("enable-memory", false, "copilot only: turn on session memory")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-attempt deadline (45m default with --tools)")
	format := fs.String("format", "text", "stream text or print the JSON result envelope")
	rubric := fs.String("rubric", "", "review standard file quoted verbatim ahead of the task")
	retry := fs.String("retry", "", "replay a logged dispatch by id (or unique id prefix); see --log")
	showLog := fs.Bool("log", false, "list the 20 most recent dispatches and exit")
	background := fs.Bool("background", false, "detach: log a job, print its id, exit")
	job := fs.String("job", "", "internal: run saved dispatch <id> in place")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := validateAskFlags(*format, *fallback); err != nil {
		return fail("ask: %v", err)
	}
	if *background && *job != "" {
		return fail("ask: --background and --job are mutually exclusive")
	}
	if *showLog {
		return printDispatchLog(*format)
	}
	prompt := strings.Join(fs.Args(), " ")
	state := &askState{name: name, provider: provider, model: model, effort: effort, workdir: workdir,
		session: session, tools: tools, timeout: timeout, attach: &attach, keepEnv: &keepEnv,
		autonomous: autonomous, enableMemory: enableMemory, rubric: rubric}
	jobRec, rc, done := loadAskJob(*job, state, &prompt)
	if done {
		return rc
	}
	if jobRec == nil {
		if rc, done := loadAskRetry(fs, args, *retry, state, &prompt); done {
			return rc
		}
	}
	prompt, err := resolveAskPrompt(prompt)
	if err != nil {
		return fail("ask: %v", err)
	}
	if strings.TrimSpace(prompt) == "" {
		fs.Usage()
		return 2
	}
	applyProfileRow(*name, provider, model, effort)
	r, chain, err := resolveAskChain(*name, *provider, *model, prompt, *fallback, *native, *workdir)
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
	if *retry == "" && *job == "" && *tools && !timeoutExplicit(fs) {
		*timeout = 45 * time.Minute
	}
	if *enableMemory && *name != "copilot" {
		fmt.Fprintf(os.Stderr, "ask: --enable-memory has no effect on the %s runner (copilot only)\n", *name)
	}
	req := agent.Request{Prompt: prompt, Model: *model, Provider: *provider, Effort: *effort,
		WorkDir: *workdir, SessionID: *session, Tools: *tools, Timeout: *timeout,
		Attach: []string(attach), KeepEnv: []string(keepEnv), Autonomous: *autonomous, EnableMemory: *enableMemory}
	rec := buildAskRecord(ctx, r, req, jobRec, *workdir, *background)
	_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run
	if *background {
		if err := vendoragent.Spawn(&rec, backgroundExtraFlags(fs, *fallback, *native)); err != nil {
			return fail("ask: %v", err)
		}
		reportBackgroundStart(*format, rec)
		return 0
	}
	return runAskAndReport(ctx, &rec, chain, req, stream, jobRec, *format)
}

// buildAskRecord constructs the dispatch record for req, reusing jobRec's identity when resuming a job.
func buildAskRecord(ctx context.Context, r agent.Runner, req agent.Request, jobRec *agent.Record, workdir string, background bool) agent.Record {
	rec := agent.NewRecord(r.Name(), req, time.Now())
	if jobRec != nil {
		return *jobRec // same id: a job run continues the record it was spawned from
	}
	cwd := workdir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	rec.Repo = vendoragent.RepoRoot(ctx, cwd)
	rec.Owner = inflight.SessionOwner()
	rec.Log = rec.LogPath()
	if !background {
		rec.PID = os.Getpid() // foreground run: alive while this process runs, lost only if it dies
	}
	return rec
}

// runAskAndReport runs chain in the foreground, logs the result into rec, and reports it.
func runAskAndReport(ctx context.Context, rec *agent.Record, chain agent.Chain, req agent.Request,
	stream io.Writer, jobRec *agent.Record, format string) int {
	start := time.Now()
	notify := func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) }
	sink := agent.NewLogSink(rec.LogPath())
	runCtx := agent.WithLog(ctx, sink)
	if jobRec != nil {
		// A --job child's stdout is the log (Spawn): event lines stream live, the full answer lands once at the end.
		runCtx, stream = agent.WithLogEvents(ctx, sink), io.Discard
	}
	res, err := agent.RunWithFallback(runCtx, chain, req, stream, notify)
	sink.Close() // flush before the record leaves running, so tail -f never misses the last line
	if jobRec != nil && res.Answer != "" && format != "json" {
		fmt.Print(res.Answer) // `<vendor> result` reads the log; ▸ lines are capped, so the answer goes in whole
	}
	rec.Finish(res, err, time.Since(start))
	_ = rec.Save()
	reportAskResult(format, res, err)
	fmt.Fprintln(os.Stderr, "ask: dispatch", rec.ID, "(claude-code ask --retry <id> replays it)")
	if err != nil {
		return fail("ask: %v", err)
	}
	return 0
}

// askState bundles the ask flag pointers that --job/--retry overwrite from a logged dispatch's request.
type askState struct {
	name, provider, model, effort, workdir, session, rubric *string
	tools, autonomous, enableMemory                         *bool
	timeout                                                 *time.Duration
	attach                                                  *attachFlag
	keepEnv                                                 *keepEnvFlag
}

// applyRecord copies a logged dispatch's request onto this ask invocation's flags.
func (s *askState) applyRecord(rec agent.Record) {
	*s.name, *s.provider, *s.model, *s.effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
	*s.workdir, *s.session, *s.tools, *s.timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
	*s.attach, *s.autonomous, *s.enableMemory = attachFlag(rec.Request.Attach), rec.Request.Autonomous, rec.Request.EnableMemory
	*s.keepEnv = keepEnvFlag(rec.Request.KeepEnv)
}

// loadAskJob loads a --job record for a background run continuation; rc/done are set only on an early return.
func loadAskJob(job string, s *askState, prompt *string) (rec *agent.Record, rc int, done bool) {
	if job == "" {
		return nil, 0, false
	}
	r, err := agent.LoadRecord(job)
	if err != nil {
		return nil, fail("ask: %v", err), true
	}
	if r.Status != "running" || r.PID != 0 {
		fail("ask --job %s: record is %s (pid %d), not a fresh background job", job, r.Status, r.PID)
		return nil, 2, true
	}
	s.applyRecord(r)
	*prompt, *s.rubric = r.Request.Prompt, ""
	r.PID = os.Getpid()
	return &r, 0, false
}

// loadAskRetry loads a --retry record, then re-applies any flags explicitly typed on this command line.
func loadAskRetry(fs *flag.FlagSet, args []string, retry string, s *askState, prompt *string) (rc int, done bool) {
	if retry == "" {
		return 0, false
	}
	rec, err := agent.LoadRecord(retry)
	if err != nil {
		return fail("ask: %v", err), true
	}
	// The logged request wins; only flags typed on this command line override it.
	s.applyRecord(rec)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "attach" {
			*s.attach = nil
		}
		if f.Name == "keep-env" {
			*s.keepEnv = nil
		}
	})
	if err := fs.Parse(args); err != nil { // re-apply explicit flags over the record
		return 2, true
	}
	*prompt, *s.rubric = rec.Request.Prompt, ""
	fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	return 0, false
}

// attachFlag collects repeatable --attach paths; Set resolves each to an absolute path and fails fast
// unless it names an existing regular file.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

// Placeholder names the FLAGS placeholder for attachFlag (helptext.Placeholder).
func (a *attachFlag) Placeholder() string { return "PATH" }

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
	// reject binary up front: it fails the same way on every runner, so it must never reach the fallback chain
	if data, err := os.ReadFile(abs); err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	} else if !agent.IsText(data) {
		return fmt.Errorf("--attach %q: binary file; attach text only", s)
	}
	*a = append(*a, abs)
	return nil
}

// keepEnvFlag collects repeatable --keep-env NAME[,NAME] values, comma-split per occurrence.
type keepEnvFlag []string

func (k *keepEnvFlag) String() string { return strings.Join(*k, ",") }

// Placeholder names the FLAGS placeholder for keepEnvFlag (helptext.Placeholder).
func (k *keepEnvFlag) Placeholder() string { return "NAME,NAME" }

func (k *keepEnvFlag) Set(s string) error {
	*k = append(*k, strings.Split(s, ",")...)
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

// applyProfileRow fills the unset provider/model/effort from the active profile's row for the agent; explicit
// flags win, and with no active profile (or no row) nothing changes.
func applyProfileRow(name string, provider, model, effort *string) {
	reg, err := providers.Load()
	if err != nil {
		return
	}
	b := reg.ResolveAgent(providers.AgentRequest{Agent: name, Provider: *provider, Model: *model, Effort: *effort})
	if *provider != "" || !strings.Contains(*model, ":") {
		*provider = b.Provider // a provider:model flag stays unsplit for the explicit-backend path
	}
	*model, *effort = b.Model, b.Effort
}

// resolveAskChain looks up the named runner and routes it: an explicit backend (--provider, provider:model) or the
// api runner runs alone; otherwise the tier comes from --model, else the task classifier, and route picks the legs.
// workDir is the request's own --workdir ("" means the process cwd), so the opencode free-Zen gate checks the
// repo the leg will actually run in, not the harness's own cwd.
func resolveAskChain(name, provider, model, prompt, fallback string, native bool, workDir string) (agent.Runner, agent.Chain, error) {
	r, err := agent.Lookup(name)
	if err != nil {
		return nil, agent.Chain{}, err
	}
	if agent.ExplicitBackend(agent.Request{Provider: provider, Model: model}) || name == "api" {
		req := agent.Request{Provider: provider, Model: model}
		if provider == "" || native || !agent.IsHubAgent(name) {
			return r, agent.ExplicitChain(r, req), nil
		}
		reg, err := providers.Load()
		if err != nil {
			return nil, agent.Chain{}, err
		}
		if p, perr := reg.Get(provider); perr == nil && p.OpencodeID == "" {
			return r, agent.ExplicitChain(r, req), nil // no opencode id: the hub can't route it; run native
		}
		start, err := agent.HubStart(reg, name, provider)
		if err != nil {
			return nil, agent.Chain{}, err
		}
		return r, agent.ExplicitChain(agent.NewHubLeg(name, start, reg), req), nil
	}
	tb, err := route.Load()
	if err != nil {
		return nil, agent.Chain{}, err
	}
	tier := tb.TierOf(model)
	if tier == "" {
		tier = hooks.RuleClassifier{}.Tier(prompt)
	}
	chain, _, err := agent.RouteChain(context.Background(), name, tier, fallback, model != "", native,
		route.Opts{Refresh: routeRefresh, WorkRepo: func() bool { return route.IsWorkRepo(workDir) }})
	return r, chain, err
}

// backgroundExtraFlags carries explicitly-set --fallback/--native over to the spawned --job child;
// unset flags fall back to the child's own defaults instead of being re-pinned.
func backgroundExtraFlags(fs *flag.FlagSet, fallback string, native bool) []string {
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "fallback":
			extra = append(extra, "--fallback", fallback)
		case "native":
			if native {
				extra = append(extra, "--native")
			}
		}
	})
	return extra
}

// reportBackgroundStart tells the caller how to track a detached job.
func reportBackgroundStart(format string, rec agent.Record) {
	if format == "json" {
		body, _ := json.Marshal(struct {
			ID  string `json:"id"`
			Log string `json:"log"`
		}{rec.ID, rec.Log})
		fmt.Println(string(body))
		return
	}
	fmt.Printf("job %s started — claude-code %s status|result|cancel\n", rec.ID, rec.Agent)
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

// agentsPage is `agents -h`; bare `agents` lists the runners.
var agentsPage = helptext.Page{
	Usage: "claude-code agents [<command>] [flags]",
	About: "With no command, list each agent runner, whether it is available and which " +
		"request fields (model, provider, effort, …) it honors.",
	Commands: [][2]string{
		{"sync", "project agents/ + CLAUDE.md into every vendor home; link skills"},
		{"tree", "live tree of sessions, subagents and dispatches; --watch"},
		{"reap", "mark dead running dispatches as error; drop stale markers"},
	},
	Hint: "claude-code agents <command> -h lists that command's flags.",
}

// agentsSyncPage is `agents sync -h`.
var agentsSyncPage = helptext.Page{
	Usage: "claude-code agents sync [flags]",
	About: "Render agents/ into state/projections/<vendor>/agents plus each vendor's instruction " +
		"file, install them into the vendor homes, and link skills/ into ~/.agents/skills.",
}

// agentsReapPage is `agents reap -h`.
var agentsReapPage = helptext.Page{
	Usage: "claude-code agents reap [flags]",
	About: "Mark dispatch records still \"running\" whose process is gone as error, and drop stale " +
		"agent-inflight markers.",
}

func cmdAgents(args []string) int {
	if len(args) == 0 {
		return listRunners()
	}
	switch args[0] {
	case "sync":
		return cmdAgentsSync(args[1:])
	case "tree":
		return agenttree.Run(args[1:], paths.ConfigDir(), os.Stdout, os.Stderr)
	case "reap":
		return cmdAgentsReap(args[1:])
	case "-h", "--help", "help":
		agentsPage.Fprint(os.Stderr)
		return 0
	}
	fail("agents: unknown subcommand %q", args[0])
	agentsPage.Fprint(os.Stderr)
	return 1
}

// cmdAgentsReap marks dead-but-"running" dispatch records as error and drops stale agent-inflight markers.
func cmdAgentsReap(args []string) int {
	fs := helptext.NewFlagSet("agents reap", agentsReapPage)
	dryRun := fs.Bool("dry-run", false, "print what would change, write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	return agenttree.Reap(*dryRun, os.Stdout, os.Stderr)
}

// cmdAgentsSync renders agents/ into state/projections/<vendor>/agents plus each vendor's global instruction file, installs the
// copies into the vendor homes, and links skills/ into ~/.agents/skills; --vendor narrows the run to one target.
func cmdAgentsSync(args []string) int {
	fs := helptext.NewFlagSet("agents sync", agentsSyncPage)
	vendor := fs.String("vendor", "", "one target: "+strings.Join(crossgen.VendorNames(), "|")+" (default: all)")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	check := fs.Bool("check", false, "dry-run; exit 1 when the projection is stale")
	noPrune := fs.Bool("no-prune", false, "keep orphaned projections")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	return crossgen.Sync(crossgen.SyncOptions{DryRun: *dryRun, Check: *check, NoPrune: *noPrune, Vendor: *vendor, Out: os.Stdout})
}

// onPassthroughProvider reports whether reg's default target forwards the caller's own auth (a
// Claude subscription), the case api.go delegates to claude instead of calling a vendor API.
func onPassthroughProvider(reg *providers.Registry) bool {
	sel, err := providers.Select(reg, "")
	return err == nil && sel.Provider != nil && sel.Provider.Auth.Type == providers.AuthPassthrough
}

func listRunners() int {
	// Loaded once and reused below: apiRunner.Available() would otherwise reopen the vault a second
	// time in the same iteration just to re-derive the same passthrough fact for the reason text.
	reg, regErr := providers.Load()
	for _, r := range agent.Runners() {
		mark, reason := "✓", ""
		if r.Name() == "api" {
			switch {
			case regErr != nil:
				mark, reason = "✗", regErr.Error()
			case onPassthroughProvider(reg):
				if claude, err := agent.Lookup("claude"); err != nil || claude.Available() != nil {
					mark, reason = "✗", "runs via claude on passthrough providers, but claude is unavailable"
				} else {
					reason = "runs via claude on passthrough providers (no key to call the vendor directly)"
				}
			}
		} else if err := r.Available(); err != nil {
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
		preview := agent.Preview(r.Request.Prompt, 60)
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
