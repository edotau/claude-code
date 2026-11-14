package ask

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// askFlags are the values of ask's flags.
type askFlags struct {
	name, fallback, provider, as, model, effort, workdir, session, debugFile, systemPrompt, appendSystemPrompt, outputSchema, host, format, rubric, retry, job, batchSrc, overlay, overlayDebug string
	native, tools, readOnly, noTools, noPersist, safeMode, direct, autonomous, fleet, enableMemory, stdinFramed, live, background, failFast                                                     bool
	timeout                                                                                                                                                                                     time.Duration
	jobs, maxFiles, maxFileBytes                                                                                                                                                                int
	dirs, files                                                                                                                                                                                 string
	diff, index, withTests                                                                                                                                                                      bool
	pack                                                                                                                                                                                        *pack.Spec
	agents                                                                                                                                                                                      agentsFlag
	attach                                                                                                                                                                                      attachFlag
	keepEnv                                                                                                                                                                                     []string
	permAllow, permAsk, permDeny                                                                                                                                                                []string
	permMode                                                                                                                                                                                    string
	leadProvider                                                                                                                                                                                string
	env                                                                                                                                                                                         []string
}

// CompletionFlags lists ask's flags for tab completion; --format stays free text, as text|json would drop stream-json.
func CompletionFlags() []helptext.Flag {
	fs, _ := askFlagSet()
	return helptext.FlagsOf(fs, map[string]func() []string{"agent": engineNames, "profile": providers.ProfileNames,
		"mode": func() []string { return []string{"default", "auto", "plan"} }, "format": nil,
		"fallback": func() []string { return router.FallbackModes }, "as": func() []string { return []string{"user", "sp"} }})
}

// engineNames lists the `ask --agent` agent names; agent.Engines() only builds the roster, it never dials an agent.
func engineNames() []string {
	var out []string
	for _, r := range agent.Engines() {
		out = append(out, r.Name())
	}
	return out
}

func askFlagSet() (*flag.FlagSet, *askFlags) {
	fs := helptext.NewFlagSet("ask", askPage)
	f := &askFlags{}
	fs.StringVar(&f.name, "agent", "claude", "agent, agents/<name>.md, <agent>:<name>, or an --agents key")
	fs.Var(&f.agents, "agents", "agent definitions: --agent takes their keys")
	registerFallbackFlag(fs, f)
	fs.BoolVar(&f.native, "native", false, "opencode: skip the hub ladder; run on its default login")
	fs.StringVar(&f.provider, "profile", "", "profile, or session; leads, then the pin (--fallback none pins)")
	fs.StringVar(&f.leadProvider, "provider", "", "lead on this provider's signed-in profile; falls back like --fallback")
	fs.StringVar(&f.as, "as", "", "claude only: auto|user|sp (default: the profile's)")
	fs.StringVar(&f.model, "model", "", "alias (opus), version (opus-5-5) or wire id")
	fs.StringVar(&f.effort, "effort", "", "reasoning effort: "+strings.Join(agentid.EffortNames(), "|"))
	fs.StringVar(&f.workdir, "workdir", "", "working directory for the agent")
	fs.StringVar(&f.session, "session", "", "resume this session id")
	fs.BoolVar(&f.tools, "tools", false, "allow tool use where the agent can gate it (default: disabled)")
	fs.BoolVar(&f.readOnly, "read-only", false, "reads and a vetted read-only shell list (codex: tools off)")
	registerPermFlags(fs, f)
	fs.BoolVar(&f.noTools, "no-tools", false, "claude only: answer from the profile directly, no tools; not with --tools")
	fs.BoolVar(&f.noPersist, "no-persist", false, "claude, codex: do not save the session for later resume")
	fs.BoolVar(&f.safeMode, "safe-mode", false, "claude, codex, opencode: skip hooks, plugins, customizations")
	fs.StringVar(&f.debugFile, "debug-file", "", "claude, gemini: write the agent's debug log to this path")
	fs.Var(&f.attach, "attach", "attach a file (repeatable); task text precedes every attachment")
	helptext.ListVar(fs, &f.keepEnv, "keep-env", "NAME,NAME", "keep NAME[,NAME] from the credential scrub (repeatable)", true)
	helptext.ListVar(fs, &f.env, "env", "NAME=VALUE", "agent env NAME=VALUE (repeatable); no credentials", false)
	askRunFlags(fs, f)
	return fs, f
}

// registerFallbackFlag, registerPermFlags and registerOverlayFlag are the flag definitions ask and dispatch share.
func registerFallbackFlag(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.fallback, "fallback", "", "all|agents|none; all if "+router.EnvFallback+" set")
}

func registerPermFlags(fs *flag.FlagSet, f *askFlags) {
	helptext.ListVar(fs, &f.permAllow, "allow", "RULE", "allow a tool call, e.g. 'Bash(ls:*)' (repeatable)", false)
	helptext.ListVar(fs, &f.permAsk, "ask", "RULE", "ask before a tool call matching the rule (repeatable)", false)
	helptext.ListVar(fs, &f.permDeny, "deny", "RULE", "deny a tool call matching the rule (repeatable)", false)
	fs.StringVar(&f.permMode, "mode", "", "permission mode for unmatched calls: default|auto|plan")
}

func registerOverlayFlag(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.overlay, "overlay", "", "config: file, JSON or -; else $"+agentconfig.OverlayVar)
}

func askRunFlags(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.systemPrompt, "system-prompt", "", "replace the system prompt (gemini: as a preamble)")
	fs.StringVar(&f.appendSystemPrompt, "append-system-prompt", "", "append to the system prompt (else a preamble)")
	fs.StringVar(&f.outputSchema, "output-schema", "", "JSON Schema (file or inline) the answer must satisfy; envelope gains structured")
	fs.BoolVar(&f.direct, "direct", false, "agents: hand the CLI the profile route, skip the proxy")
	fs.BoolVar(&f.autonomous, "autonomous", false, "run without stopping to ask (not codex)")
	fs.BoolVar(&f.fleet, "fleet", false, "copilot only: run the task in Copilot's fleet mode (parallel subagents)")
	fs.BoolVar(&f.enableMemory, "enable-memory", false, "copilot only: turn on session memory")
	fs.DurationVar(&f.timeout, "timeout", 10*time.Minute, "per-attempt deadline (45m default with --tools)")
	fs.BoolVar(&f.stdinFramed, "stdin-framed", false, "internal: prompt is stdin up to a NUL byte; the run is cancelled when stdin then closes")
	fs.StringVar(&f.host, "host", "", "ssh host (alias, tailnet, auto); confinement rides")
	fs.StringVar(&f.format, "format", "text", "stream text, print the JSON result envelope, or stream-json NDJSON events")
	fs.StringVar(&f.dirs, "dirs", "", "comma-separated dirs whose tracked files are inlined (never .env*, state/, keys)")
	fs.StringVar(&f.files, "files", "", "comma-separated file globs to inline")
	fs.BoolVar(&f.diff, "diff", false, "inline the files changed vs HEAD")
	fs.BoolVar(&f.index, "index", false, "prepend an index of every matched path")
	fs.IntVar(&f.maxFiles, "max-files", pack.DefaultMaxFiles, "max files to inline (0 = index only)")
	fs.IntVar(&f.maxFileBytes, "max-file-bytes", pack.DefaultMaxFileBytes, "max bytes per inlined file")
	fs.BoolVar(&f.withTests, "with-tests", false, "inline test files and testdata under --dirs (skipped by default)")
	fs.StringVar(&f.rubric, "rubric", "", "review standard file quoted verbatim ahead of the task")
	fs.StringVar(&f.retry, "retry", "", "replay a logged dispatch by id (or unique id prefix); see agents status")
	fs.BoolVar(&f.live, "live", false, "claude|gemini: hold a session open for agents send; --timeout is its idle limit")
	fs.BoolVar(&f.background, "background", false, "detach: log a job, print its id, exit")
	fs.StringVar(&f.job, "job", "", "internal: run saved dispatch <id> in place")
	fs.StringVar(&f.batchSrc, "batch", "", "run JSONL tasks from <file> or - on a pool; one envelope per line")
	fs.IntVar(&f.jobs, "jobs", defaultJobs(), "batch: tasks run at once")
	fs.BoolVar(&f.failFast, "fail-fast", false, "batch: the first failed task cancels the rest")
	registerOverlayFlag(fs, f)
	fs.StringVar(&f.overlayDebug, "overlay-debug", "", "log child env names + salted hashes, never values")
}

// askCheckFlags runs the flag checks needing no resolved agent; every failure is a usage error (exit 2).
func askCheckFlags(env, keepEnv []string, format, fallback string, background bool, job string, tools, noTools bool) error {
	if _, err := agentconfig.FlagOverlay(env, keepEnv); err != nil { // names only: the error never carries a value
		return err
	}
	if err := validateAskFlags(format, fallback); err != nil {
		return err
	}
	return askExclusive(background, job, tools, noTools)
}

// askEnableOverlayDebug turns on --overlay-debug; it returns the exit code of a failure, else 0.
func askEnableOverlayDebug(debugPath string) int {
	if debugPath != "" {
		if strings.HasPrefix(debugPath, "-") {
			fmt.Fprintf(os.Stderr, "claude-code agents ask: --overlay-debug: %q looks like a flag; give a file path\n", debugPath)
			return 2
		}
		if err := agentconfig.EnableOverlayDebug(debugPath); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code agents ask: --overlay-debug:", err)
			return 2
		}
	}
	return 0
}

// askPack builds the pack spec from --dirs/--files/--diff; nil when none is set. The tuning flags need one.
func askPack(fs *flag.FlagSet, f *askFlags) (*pack.Spec, error) {
	if f.dirs == "" && f.files == "" && !f.diff {
		return nil, packTuningUnset(fs)
	}
	if err := packCombos(f); err != nil {
		return nil, err
	}
	return &pack.Spec{Dirs: textutil.SplitCSV(f.dirs), Files: textutil.SplitCSV(f.files), Diff: f.diff, Index: f.index,
		MaxFiles: f.maxFiles, MaxFileBytes: f.maxFileBytes, WithTests: f.withTests, IndexOnly: f.maxFiles == 0}, nil
}

// packCombos refuses negative pack limits and a pack with --batch, --live or --host.
func packCombos(f *askFlags) error {
	switch {
	case f.maxFiles < 0 || f.maxFileBytes < 0:
		return errors.New("--max-files and --max-file-bytes cannot be negative")
	case f.batchSrc != "" || f.live || f.host != "":
		return errors.New("--dirs/--files/--diff cannot be combined with --batch, --live or --host")
	}
	return nil
}

// packTuningUnset refuses a pack tuning flag typed without --dirs, --files or --diff.
func packTuningUnset(fs *flag.FlagSet) error {
	for _, n := range []string{"index", "max-files", "max-file-bytes", "with-tests"} {
		if helptext.Explicit(fs, n) {
			return fmt.Errorf("--%s needs --dirs, --files or --diff", n)
		}
	}
	return nil
}

// askCheckCombos rejects the flag combinations that need the resolved agent.
func askCheckCombos(fs *flag.FlagSet, name string, live, hasPersona bool, host string, background bool, job, batchSrc string, hasEnv bool) error {
	if live {
		if err := askLiveCheck(fs, name, hasPersona); err != nil {
			return err
		}
	}
	return askHostEnvCombos(host, background, job, batchSrc, hasEnv)
}

// askHostEnvCombos rejects --host with --background/--job/--batch, and --env with --background.
func askHostEnvCombos(host string, background bool, job, batchSrc string, hasEnv bool) error {
	if host != "" && (background || job != "" || batchSrc != "") {
		return errors.New("--host cannot be combined with --background, --job or --batch")
	}
	if hasEnv && background {
		return errors.New("--env cannot be combined with --background: env values are never stored in the dispatch record")
	}
	return nil
}

// attachFlag collects repeatable --attach paths; Set makes each absolute and refuses all but a regular text/image file.
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
	// reject non-image binary up front: it fails the same way on every agent, so it must never reach the fallback chain
	if data, err := readPrefix(abs, sniffBytes); err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	} else if !agent.IsText(trimPartialRune(data, info.Size() > sniffBytes)) && !agent.IsImage(data) {
		return fmt.Errorf("--attach %q: binary file; attach text only", s)
	}
	*a = append(*a, abs)
	return nil
}

// sniffBytes is how much of an --attach file is read to tell text, image and binary apart.
const sniffBytes = 64 << 10

// maxSchemaBytes caps an --output-schema file.
const maxSchemaBytes = 1 << 20

// readPrefix reads at most n bytes of path.
func readPrefix(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(n)))
}

// trimPartialRune drops a multi-byte rune that a prefix cut in half, so IsText judges only whole runes.
func trimPartialRune(data []byte, cut bool) []byte {
	if !cut {
		return data
	}
	for i := len(data) - 1; i >= 0 && i > len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if utf8.FullRune(data[i:]) {
				return data
			}
			return data[:i]
		}
	}
	return data
}

// agentsFlag is the --agents value: inline JSON or a file of agent definitions.
type agentsFlag string

func (a *agentsFlag) String() string { return string(*a) }

// Placeholder names the FLAGS placeholder for agentsFlag (helptext.Placeholder).
func (a *agentsFlag) Placeholder() string { return "JSON|FILE" }

// Set accepts anything: the flag parser echoes a rejected value in its error, so Run validates after Parse.
func (a *agentsFlag) Set(s string) error {
	*a = agentsFlag(s)
	return nil
}

// validateAskFlags checks --format and --fallback against their allowed values.
func validateAskFlags(format, fallback string) error {
	if !slices.Contains(agent.Formats, format) {
		return fmt.Errorf("--format %q unknown (text|json|stream-json)", format)
	}
	if !slices.Contains(router.FallbackModes, fallback) {
		return fmt.Errorf("--fallback %q unknown (%s)", fallback, strings.Join(router.FallbackModes, "|"))
	}
	return nil
}

// askExclusive rejects flag pairs that cannot be combined.
func askExclusive(background bool, job string, tools, noTools bool) error {
	if background && job != "" {
		return errors.New("--background and --job are mutually exclusive")
	}
	if tools && noTools {
		return errors.New("--tools and --no-tools are mutually exclusive")
	}
	return nil
}

// askReselect re-resolves the agent after --job/--retry; it returns the new selector and agent.
func askReselect(ref, ag string, name *string, persona **agent.Schema, defs map[string]*agent.Schema) (string, string, error) {
	if *name != ag { // --job/--retry replaced the agent: resolve the record's (or re-typed) value instead
		ref = *name
	}
	rest, p, err := agent.SelectAgent(ref, defs) // a --retry re-parse puts the raw --agent value back
	if err != nil {
		return ref, ag, err
	}
	*name = rest
	if p != nil { // an explicit persona beats the record's
		*persona = p
	}
	return ref, rest, nil
}

// readFramedPrompt reads the prompt up to its ending NUL; the returned reader's remaining EOF is the cancel signal.
func readFramedPrompt(in io.Reader) (*bufio.Reader, string, error) {
	rd := bufio.NewReader(in)
	p, err := rd.ReadString(0)
	if err != nil {
		return nil, "", fmt.Errorf("--stdin-framed: stdin ended before the NUL that ends the prompt: %w", err)
	}
	return rd, strings.TrimSuffix(p, "\x00"), nil
}

// resolveAskPrompt reads stdin when the prompt is empty or the literal "-".
func resolveAskPrompt(prompt string) (string, error) {
	if prompt != "" && prompt != "-" {
		return prompt, nil
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, agent.MaxPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(raw) > agent.MaxPromptBytes {
		return "", agent.PromptTooLarge(len(raw), agent.MaxPromptBytes)
	}
	return string(raw), nil
}

// resolveOutputSchema reads --output-schema as inline JSON when it starts with "{", else a file, and checks it parses.
func resolveOutputSchema(arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	body := strings.TrimSpace(arg)
	if !strings.HasPrefix(body, "{") {
		data, err := readPrefix(arg, maxSchemaBytes+1)
		if err != nil {
			return "", fmt.Errorf("--output-schema: %w", err)
		}
		if len(data) > maxSchemaBytes {
			return "", fmt.Errorf("--output-schema: %s is over the %d byte limit", arg, maxSchemaBytes)
		}
		body = strings.TrimSpace(string(data))
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return "", fmt.Errorf("--output-schema: not a JSON object: %w", err)
	}
	if probe == nil { // "null" decodes into a nil map without error
		return "", errors.New("--output-schema: not a JSON object: got null")
	}
	return body, nil
}

// askTarget bundles the ask flag pointers that a logged dispatch's request overwrites on --job/--retry.
type askTarget struct {
	Name, Provider, Model, Effort, Workdir, Session                   *string
	Tools, ReadOnly, NoTools, Autonomous, Fleet, EnableMemory, Direct *bool
	NoPersist, SafeMode, Native                                       *bool
	DebugFile, SystemPrompt, AppendSystemPrompt, OutputSchema         *string
	Host, Fallback, Coworker                                          *string
	Timeout                                                           *time.Duration
	Persona                                                           **agent.Schema
	Pack                                                              **pack.Spec
	Attach, KeepEnv                                                   *[]string
	PermMode                                                          *string
	PermAllow, PermAsk, PermDeny                                      *[]string
}

// Apply copies a logged dispatch's request onto this ask invocation's flags; the env-names note goes to errw.
func (t *askTarget) Apply(rec agent.Record, errw io.Writer) {
	*t.Name, *t.Provider, *t.Model, *t.Effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
	*t.Workdir, *t.Session, *t.Tools, *t.Timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
	*t.Attach, *t.Autonomous, *t.Fleet, *t.EnableMemory = rec.Request.Attach, rec.Request.Autonomous, rec.Request.Fleet, rec.Request.EnableMemory
	*t.KeepEnv, *t.Direct, *t.NoTools, *t.ReadOnly = rec.Request.KeepEnv, rec.Request.Direct, rec.Request.NoTools, rec.Request.ReadOnly
	*t.NoPersist, *t.SafeMode, *t.DebugFile = rec.Request.NoPersist, rec.Request.SafeMode, rec.Request.DebugFile
	*t.SystemPrompt, *t.AppendSystemPrompt, *t.OutputSchema = rec.Request.SystemPrompt, rec.Request.AppendSystemPrompt, rec.Request.OutputSchema
	*t.Host = rec.Host
	p := rec.Request.Permissions
	*t.PermMode, *t.PermAllow, *t.PermAsk, *t.PermDeny = p.Mode, agent.RuleStrings(p.Allow), agent.RuleStrings(p.Ask), agent.RuleStrings(p.Deny)
	*t.Native, *t.Pack, *t.Persona = rec.Request.Native, rec.Request.Pack, rec.Request.Persona
	if rec.FallbackPolicy != "" { // records from before the field keep this run's default
		*t.Fallback = rec.FallbackPolicy
	}
	if rec.Route != "" { // the workspace the run was confined to; an overlay provider still wins (checkArm)
		*t.Coworker = rec.Route
	}
	if len(rec.Request.EnvNames) > 0 {
		fmt.Fprintf(errw, "ask: the logged run set env %s; values are not stored, so pass --env or --overlay again\n", strings.Join(rec.Request.EnvNames, ","))
	}
}

// askState is the askTarget plus the flag pointers --retry resets or --job clears.
type askState struct {
	askTarget
	rubric *string
	env    *[]string
	lead   *string // routed lead profile resolved by resolveLead; a live holder keeps it
}

// askTimeout is the deadline: a fresh --tools run without an explicit --timeout gets 45m.
func askTimeout(fs *flag.FlagSet, timeout time.Duration, retry, job string, tools bool) time.Duration {
	if retry == "" && job == "" && tools && !helptext.Explicit(fs, "timeout") {
		return agent.DefaultToolsTimeout
	}
	return timeout
}

// loadAskJob loads a --job record for a background run continuation; rc/done are set only on an early return.
func loadAskJob(job, format string, s *askState, prompt *string) (rec *agent.Record, rc int, done bool) {
	if job == "" {
		return nil, 0, false
	}
	r, err := loadJobRecord(job)
	if errors.Is(err, errJobNotFresh) {
		return nil, failAskCode(format, *s.Name, err, 2), true
	}
	if err != nil {
		return nil, failAsk(format, *s.Name, err), true
	}
	s.Apply(r, os.Stderr)
	*prompt, *s.rubric = r.Request.Prompt, ""
	return &r, 0, false
}

// loadAskRetry loads a --retry record, then re-applies any flags explicitly typed on this command line.
func loadAskRetry(ctx context.Context, fs *flag.FlagSet, args []string, retry, format string, s *askState, prompt *string) (rc int, done bool) {
	if retry == "" {
		return 0, false
	}
	rec, err := agent.LoadRecord(retry)
	if err != nil {
		return failAsk(format, *s.Name, err), true
	}
	// The logged request wins; only flags typed on this command line override it.
	s.Apply(rec, os.Stderr)
	*s.lead = rec.Request.Lead // the routed lead is not a flag: the replay leads with the profile the run used
	clearTyped(fs, s)
	if err := fs.Parse(args); err != nil { // re-apply explicit flags over the record
		return failAskCode(format, *s.Name, err, 2), true
	}
	if !helptext.Explicit(fs, "fallback") && strings.TrimSpace(proc.Value(ctx, router.EnvFallback)) != "" {
		*s.Fallback = router.FallbackAll
	}
	*prompt, *s.rubric = rec.Request.Prompt, ""
	fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	return 0, false
}

// clearTyped drops the record's lead and list values for flags typed on this command line; the re-parse sets them.
func clearTyped(fs *flag.FlagSet, s *askState) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "profile", "provider": // an explicit route on this command line replaces the logged lead
			*s.lead = ""
		case "attach":
			*s.Attach = nil
		case "keep-env":
			*s.KeepEnv = nil
		case "allow":
			*s.PermAllow = nil
		case "ask":
			*s.PermAsk = nil
		case "deny":
			*s.PermDeny = nil
		case "env":
			*s.env = nil
		}
	})
}

// errJobNotFresh marks a --job record that is not a fresh background job.
var errJobNotFresh = errors.New("not a fresh background job")

// loadJobRecord loads a --job record for a background run continuation and claims it for this process.
func loadJobRecord(id string) (agent.Record, error) {
	r, err := agent.LoadRecord(id)
	if err != nil {
		return r, err
	}
	if r.Status != "running" || r.PID != 0 {
		return r, fmt.Errorf("--job %s: record is %s (pid %d), %w", id, r.Status, r.PID, errJobNotFresh)
	}
	r.PID = os.Getpid()
	return r, nil
}

// resolveOverlay resolves the overlay's vault refs, failing closed; --background leaves that to its --job child.
func resolveOverlay(ov agentconfig.Overlay, env, keep []string, background bool) (agentconfig.Resolved, error) {
	if _, err := agentconfig.FlagOverlay(env, keep); err != nil {
		return agentconfig.Resolved{}, err
	}
	if background {
		return agentconfig.Resolved{}, nil
	}
	return agentconfig.ResolveOverlay(ov)
}

// overlayLocal refuses an overlay's env layer on --live or --host: neither carries it; routing keys alone pass.
func overlayLocal(ov agentconfig.Overlay, live bool, host string) error {
	if !ov.Empty() && (live || host != "") {
		return errors.New("overlay set/unset/keep cannot reach --live or --host: run the agent locally")
	}
	return nil
}

// parseAgentDefs parses --agents; --host cannot carry an inline definition.
func parseAgentDefs(agents, host string) (map[string]*agent.Schema, error) {
	var defs map[string]*agent.Schema
	if agents != "" {
		var err error
		if defs, err = agent.ParseAgents(agents); err != nil {
			return nil, err
		}
	}
	if host != "" && agents != "" {
		return nil, errors.New("--host cannot be combined with --agents: the remote cannot see an inline definition")
	}
	return defs, nil
}

// absDebugFile pins the path now: --workdir moves the child's cwd.
func absDebugFile(p string) string {
	if p == "" {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
