package ask

import (
	"bufio"
	"cmp"
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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/pi"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy/translate"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// askFlags are the values of ask's flags.
type askFlags struct {
	name, fallback, profile, as, tier, model, effort, workdir, session, debugFile, systemPrompt, appendSystemPrompt, outputSchema, host, format, rubric, retry, job, batchSrc, overlay string
	modelTyped, timeoutTyped                                                                                                                                                           bool
	recover                                                                                                                                                                            bool
	native, tools, noTools, noPersist, direct, autonomous, fleet, enableMemory, stdinFramed, live, background, failFast, carry                                                         bool
	timeout                                                                                                                                                                            time.Duration
	jobs, maxFiles, maxFileBytes                                                                                                                                                       int
	dirs, files, save                                                                                                                                                                  string
	diff, index                                                                                                                                                                        bool
	pack                                                                                                                                                                               *pack.Spec
	agents                                                                                                                                                                             agentsFlag
	attach                                                                                                                                                                             attachFlag
	keepEnv                                                                                                                                                                            []string
	codemode                                                                                                                                                                           string
	googleTools                                                                                                                                                                        string
	codemodeReason                                                                                                                                                                     string
	runner                                                                                                                                                                             string
	provider                                                                                                                                                                           string
	env                                                                                                                                                                                []string
	spec, verify, action                                                                                                                                                               string
	actionOwn                                                                                                                                                                          []string
	contract                                                                                                                                                                           *contractOpen
	planTask                                                                                                                                                                           string
	dryRun                                                                                                                                                                             bool
	loop                                                                                                                                                                               bool
	waves                                                                                                                                                                              string
	loopPlanner, loopResume                                                                                                                                                            string
	loopRounds, loopBudget                                                                                                                                                             int
}

// CompletionFlags lists ask's flags for tab completion; --format stays free text, as text|json would drop stream-json.
func CompletionFlags() []helptext.Flag {
	fs, _ := askFlagSet()
	return helptext.FlagsOf(fs, map[string]func() []string{"agent": engineNames, "profile": providers.ProfileNames,
		"format":   nil,
		"fallback": func() []string { return route.FallbackModes }, "as": func() []string { return []string{"user", "sp"} }})
}

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
	fs.StringVar(&f.profile, "profile", "", "profile, or session; leads, then the pin (--fallback none pins)")
	fs.StringVar(&f.provider, "provider", "", "lead on this provider's signed-in profile; falls back like --fallback")
	fs.StringVar(&f.as, "as", "", "claude only: auto|user|sp (default: the profile's)")
	fs.StringVar(&f.model, "model", "", "alias (opus), version (opus-5-5) or wire id")
	fs.StringVar(&f.planTask, "plan-task", "", "plan task id this one ask implements: the record's plan_task and a spec brief's Task: line")
	fs.StringVar(&f.tier, "tier", "", "task tier: "+strings.Join(agent.TierNames(), "|")+"; heads the brief with a Tier: line, sized under --model auto")
	fs.StringVar(&f.effort, "effort", "", "reasoning effort: "+strings.Join(agentid.EffortNames(), "|"))
	fs.StringVar(&f.workdir, "workdir", "", "working directory for the agent")
	fs.StringVar(&f.session, "session", "", "resume this session id")
	fs.BoolVar(&f.tools, "tools", false, "tool use where the agent can gate it (default: on; --tools=false or --no-tools turns it off)")
	fs.BoolVar(&f.noTools, "no-tools", false, "claude only: answer from the profile directly, no tools; not with --tools")
	fs.BoolVar(&f.noPersist, "no-persist", false, "claude, codex: do not save the session for later resume")
	fs.StringVar(&f.debugFile, "debug-file", "", "claude, gemini: write the agent's debug log to this path")
	fs.Var(&f.attach, "attach", "attach a file (repeatable); task text precedes every attachment")
	helptext.ListVar(fs, &f.keepEnv, "keep-env", "NAME,NAME", "keep NAME[,NAME] from the credential scrub (repeatable)", true)
	helptext.ListVar(fs, &f.env, "env", "NAME=VALUE", "agent env NAME=VALUE (repeatable); no credentials", false)
	askRunFlags(fs, f)
	return fs, f
}

// registerFallbackFlag and registerOverlayFlag are the flag definitions ask and dispatch share.
func registerFallbackFlag(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.fallback, "fallback", "", strings.Join(route.FallbackModes, "|")+"; default from "+router.EnvFallback)
}

func registerOverlayFlag(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.overlay, "overlay", "", "config: file, JSON or -; else $"+route.OverlayVar)
}

func askRunFlags(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.systemPrompt, "system-prompt", "", "replace the system prompt (gemini: as a preamble)")
	fs.StringVar(&f.appendSystemPrompt, "append-system-prompt", "", "append to the system prompt (else a preamble)")
	fs.StringVar(&f.outputSchema, "output-schema", "", "JSON Schema (file or inline) the answer must satisfy; envelope gains structured")
	fs.BoolVar(&f.direct, "direct", false, "agents: hand the CLI the profile route, skip the proxy")
	fs.BoolVar(&f.autonomous, "autonomous", false, "run without stopping to ask (not codex)")
	fs.BoolVar(&f.fleet, "fleet", false, "copilot only: run the task in Copilot's fleet mode (parallel subagents)")
	fs.BoolVar(&f.enableMemory, "enable-memory", false, "copilot only: turn on session memory")
	fs.StringVar(&f.codemode, "codemode", "", strings.Join(agentid.CodeModeAgents(), "|")+": "+strings.Join(route.Codemodes, "|")+"; off overrides CODEMODE; on/only need --tools; only is pi's")
	fs.StringVar(&f.runner, "runner", "", strings.Join(route.Runners, "|")+": pi runner (default on Databricks) or the agent's own CLI")
	fs.StringVar(&f.googleTools, "google-tools", "", "gemini: Google built-ins, comma list (search,url,code)")
	fs.DurationVar(&f.timeout, "timeout", 9*time.Minute+30*time.Second, "per-attempt deadline, under the 10m Bash cap")
	fs.BoolVar(&f.stdinFramed, "stdin-framed", false, "internal: prompt is stdin up to a NUL byte; the run is cancelled when stdin then closes")
	fs.StringVar(&f.host, "host", "", "ssh host (alias, tailnet, auto); confinement rides")
	fs.StringVar(&f.format, "format", "text", "stream text, print the JSON result envelope, or stream-json NDJSON events")
	fs.StringVar(&f.dirs, "dirs", "", "comma-separated dirs whose tracked files are inlined (never .env*, state/, keys)")
	fs.StringVar(&f.files, "files", "", "comma-separated file globs to inline")
	fs.BoolVar(&f.diff, "diff", false, "inline the files changed vs HEAD")
	fs.BoolVar(&f.index, "index", false, "prepend an index of every matched path")
	fs.IntVar(&f.maxFiles, "max-files", pack.DefaultMaxFiles, "max files to inline (0 = index only)")
	fs.IntVar(&f.maxFileBytes, "max-file-bytes", pack.DefaultMaxFileBytes, "max bytes per inlined file")
	fs.StringVar(&f.rubric, "rubric", "", "review standard file quoted verbatim ahead of the task")
	fs.StringVar(&f.retry, "retry", "", "replay a logged dispatch by id (or unique id prefix); see agents status")
	fs.BoolVar(&f.recover, "recover", false, "with --retry: rerun a dead or cut-off job from its event log, fresh session")
	fs.BoolVar(&f.live, "live", false, "claude|gemini: hold a session open for agents send; --timeout is its idle limit")
	fs.BoolVar(&f.background, "background", false, "detach: log a job, print its id, exit")
	fs.BoolVar(&f.carry, "carry", false, "run the ask as a plain child; Pi takes over a cut-off run (row Carry)")
	fs.StringVar(&f.save, "save", "", "write the answer to FILE; stdout keeps its first 8 lines")
	fs.StringVar(&f.job, "job", "", "internal: run saved dispatch <id> in place")
	fs.StringVar(&f.batchSrc, "batch", "", "run JSONL tasks from <file> or - on a pool; one envelope per line")
	fs.IntVar(&f.jobs, "jobs", defaultJobs(), "batch: tasks run at once")
	fs.BoolVar(&f.failFast, "fail-fast", false, "batch: the first failed task cancels the rest")
	fs.BoolVar(&f.loop, "loop", false, "orchestrate the goal in rounds of parallel parts")
	fs.IntVar(&f.loopRounds, "loop-rounds", loopDefaultRounds, "loop: most rounds per run")
	fs.StringVar(&f.loopPlanner, "loop-planner", "claude", "loop: agent that plans each round")
	fs.IntVar(&f.loopBudget, "loop-budget-tokens", 0, "loop: stop past this many tokens (0 = none)")
	fs.StringVar(&f.loopResume, "loop-resume", "", "continue the saved loop <id>")
	registerOverlayFlag(fs, f)
	fs.StringVar(&f.spec, "spec", "", "the whole ask as JSON: file, inline {...} or -")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print route and argv; no model call")
	registerWavesFlags(fs, f)
}

// registerWavesFlags names the --waves flags so ask parses and completes them; Waves re-parses and runs them.
func registerWavesFlags(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.waves, "waves", "", "run the waves of this plan (--wave N[-M], --status, --fresh, --gate CMD, --profiles)")
	fs.String("wave", "", "with --waves, wave N or inclusive range N-M")
	fs.Bool("status", false, "with --waves, print saved state without launching")
	fs.Bool("fresh", false, "with --waves, discard saved state and use current HEAD")
	fs.String("gate", "", "with --waves, override the wave gate command")
	fs.String("profiles", "", "with --waves, leaf profiles (comma-separated)")
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

// directRoute is true only for an explicit Anthropic route; an unset or later-resolved route counts as a gateway.
func directRoute(provider, profile string) bool {
	return provider == "anthropic" || profile == "anthropic"
}

// validateCodemode resolves --codemode: a misspelled value errors; an unsupported one runs off with a reason.
func validateCodemode(mode, ag, runner string, tools, direct bool) (resolved, reason string, err error) {
	if mode != "" && !slices.Contains(route.Codemodes, mode) {
		return "", "", fmt.Errorf("--codemode %q must be %s", mode, strings.Join(route.Codemodes, " or "))
	}
	if ag == agentid.Pi {
		return piCodemode(mode, tools)
	}
	if mode != "" && piRunnerLeads(ag, runner, direct) { // the vault CODEMODE never turns it on for another agent's ask
		return piCodemode(mode, tools)
	}
	if mode == "" || mode == "off" {
		return "", "", nil
	}
	if !tools {
		return "off", "needs --tools: scripts call the tools", nil
	}
	if why := codemodeRowGap(agentid.Routing(ag).CodeMode, mode, direct); why != "" {
		return "off", why, nil
	}
	return mode, "", nil
}

// piRunnerLeads reports a typed agent with no code-mode row of its own running on the pi runner, which owns code mode.
func piRunnerLeads(ag, runner string, direct bool) bool {
	if runner == agentconfig.RunnerOwn || direct || !agentid.Known(ag) || agentid.Routing(ag).CodeMode.Has() {
		return false
	}
	pi, err := agent.Lookup(agentid.Pi)
	return err == nil && pi.Available() == nil
}

// piCodemode is pi's resolved mode (the vault CODEMODE under --tools); on or only without tools runs off.
func piCodemode(mode string, tools bool) (string, string, error) {
	mode, err := pi.ResolveCodemode(mode, tools)
	if err != nil || tools || (mode != "on" && mode != "only") {
		return mode, "", err
	}
	return "off", "needs --tools: scripts call the tools", nil
}

// codemodeRowGap is why the agent's row cannot give mode on this route; "" when it can.
func codemodeRowGap(row agentid.CodeModeRow, mode string, direct bool) string {
	switch {
	case !row.Has():
		return "no code mode; only " + strings.Join(agentid.CodeModeAgents(), ", ") + " have one"
	case mode == "only" && !row.Only:
		return "no script-only mode"
	case row.DirectOnly && !direct:
		return "code mode is only on the Anthropic API, not through a cloud provider or a gateway"
	}
	return ""
}

// noteCodemodeOff prints the one stderr note for a code mode that resolved to off with a reason.
func noteCodemodeOff(ag, reason string) {
	if reason != "" {
		fmt.Fprintf(os.Stderr, "codemode: off for %s (%s)\n", ag, reason)
	}
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
		MaxFiles: f.maxFiles, MaxFileBytes: f.maxFileBytes, IndexOnly: f.maxFiles == 0}, nil
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
	for _, n := range []string{"index", "max-files", "max-file-bytes"} {
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

// IsRepeatable marks --attach as a list for the spec schema.
func (a *attachFlag) IsRepeatable() bool { return true }

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
	data, err := readPrefix(abs, sniffBytes)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	text := data
	if info.Size() > sniffBytes {
		text = pack.TrimPartialRune(data)
	}
	if !agent.IsText(text) && !agent.IsImage(data) {
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
	if !slices.Contains(route.FallbackModes, fallback) {
		return fmt.Errorf("--fallback %q unknown (%s)", fallback, strings.Join(route.FallbackModes, "|"))
	}
	return nil
}

// googleToolsCheck refuses --google-tools on another agent or with an unknown or held tool.
func googleToolsCheck(f *askFlags) error {
	if f.googleTools == "" {
		return nil
	}
	if f.name != "" && f.name != agentid.Gemini {
		return fmt.Errorf("--google-tools is gemini only, not %s", f.name)
	}
	_, err := googleToolNames(f.googleTools)
	return err
}

// finalGoogleTools runs on the final agent: off gemini a record-only list is dropped with a note, a typed one refused.
func finalGoogleTools(f *askFlags, typed string) error {
	if typed == "" && f.googleTools != "" {
		var tools []string
		if tools = []string{f.googleTools}; f.name != "" {
			dropGoogleTools(f.name, &tools)
		}
		f.googleTools = strings.Join(tools, "")
	}
	return googleToolsCheck(f)
}

// dropGoogleTools clears an inherited list for an agent other than gemini, with a note.
func dropGoogleTools(name string, tools *[]string) {
	if len(*tools) > 0 && name != agentid.Gemini {
		fmt.Fprintf(os.Stderr, "google-tools: dropped for %s (gemini only)\n", name)
		*tools = nil
	}
}

// googleToolNames is the validated --google-tools list, ready for RunOptions.
func googleToolNames(csv string) ([]string, error) {
	tools, err := translate.ParseGoogleTools(strings.Split(csv, ","))
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = string(t)
	}
	return out, err
}

// runnerCheck refuses a --runner outside route.Runners.
func runnerCheck(f *askFlags) error {
	if f.runner != "" && !slices.Contains(route.Runners, f.runner) {
		return fmt.Errorf("--runner %q must be %s", f.runner, strings.Join(route.Runners, " or "))
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
	Name, Provider, Model, Effort, Workdir, Session           *string
	Tools, NoTools, Autonomous, Fleet, EnableMemory, Direct   *bool
	NoPersist, Native                                         *bool
	DebugFile, SystemPrompt, AppendSystemPrompt, OutputSchema *string
	Host, Fallback, Coworker, Codemode, Runner, Verify        *string
	GoogleTools                                               *string
	Timeout                                                   *time.Duration
	Persona                                                   **agent.Schema
	Pack                                                      **pack.Spec
	Attach, KeepEnv                                           *[]string
}

// Apply copies a logged dispatch's request onto this ask invocation's flags; the env-names note goes to errw.
func (t *askTarget) Apply(rec agent.Record, errw io.Writer) {
	*t.Name, *t.Provider, *t.Model, *t.Effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
	*t.Workdir, *t.Session, *t.Tools, *t.Timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
	*t.Attach, *t.Autonomous, *t.Fleet, *t.EnableMemory = rec.Request.Attach, rec.Request.Autonomous, rec.Request.Fleet, rec.Request.EnableMemory
	*t.KeepEnv, *t.Direct, *t.NoTools = rec.Request.KeepEnv, rec.Request.Direct, rec.Request.NoTools
	*t.NoPersist, *t.DebugFile = rec.Request.NoPersist, rec.Request.DebugFile
	*t.SystemPrompt, *t.AppendSystemPrompt, *t.OutputSchema = rec.Request.SystemPrompt, rec.Request.AppendSystemPrompt, rec.Request.OutputSchema
	*t.Host, *t.Codemode = rec.Host, rec.Request.Codemode
	if rec.Request.Runner != "" { // records from before the field keep this run's runner
		*t.Runner = rec.Request.Runner
	}
	if len(rec.Request.GoogleTools) > 0 && t.GoogleTools != nil {
		*t.GoogleTools = strings.Join(rec.Request.GoogleTools, ",")
	}
	if t.Verify != nil { // a retried or replayed run repeats its spec's verify
		*t.Verify = rec.Request.Verify
	}
	*t.Native, *t.Pack, *t.Persona = rec.Request.Native, rec.Request.Pack, rec.Request.Persona
	if rec.FallbackPolicy != "" { // records from before the field keep this run's default
		*t.Fallback = rec.FallbackPolicy
	}
	if !rec.Route.IsZero() { // the workspace the run was confined to; an overlay provider still wins (checkArm)
		*t.Coworker = route.Join(rec.Route.Provider, rec.Route.Profile)
	}
	if len(rec.Request.EnvNames) > 0 {
		fmt.Fprintf(errw, "ask: the logged run set env %s; values are not stored, so pass --env or --overlay again\n", strings.Join(rec.Request.EnvNames, ","))
	}
}

// askState is the askTarget plus the flag pointers --retry resets or --job clears.
type askState struct {
	askTarget
	rubric       *string
	env          *[]string
	modelTyped   *bool   // --model typed on this command line or in the --job/--retry record
	timeoutTyped *bool   // --timeout typed on this command line or in the --job/--retry record
	lead         *string // routed lead profile resolved by resolveLead; a live holder keeps it
	// the --job/--retry record's request: its rotation and fallback-default marks
	replayed *agent.RunOptions
}

// askTimeout is the deadline: a fresh --tools run without an explicit --timeout gets 9m30s.
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
	if errors.Is(err, agent.ErrJobNotFresh) {
		return nil, failAskCode(format, *s.Name, err, 2), true
	}
	if err != nil {
		return nil, failAsk(format, *s.Name, err), true
	}
	s.Apply(r, os.Stderr)
	*s.modelTyped, *s.timeoutTyped = r.Request.ModelTyped, r.Request.TimeoutTyped
	*s.lead, *s.replayed = r.Request.Lead, r.Request // the detached child leads with the profile the parent resolved
	*prompt, *s.rubric = r.Request.Prompt, ""
	return &r, 0, false
}

// loadAskRetry loads a --retry record, re-applies flags typed here, and marks ctx with the retried id and plan.
func loadAskRetry(ctx context.Context, fs *flag.FlagSet, args []string, retry, format string, s *askState,
	prompt *string) (_ context.Context, rc int, done bool) {
	if retry == "" {
		return ctx, 0, false
	}
	rec, err := agent.LoadRecord(retry)
	if err != nil {
		return ctx, failAsk(format, *s.Name, err), true
	}
	// The logged request wins; only flags typed on this command line override it.
	s.Apply(rec, os.Stderr)
	*s.modelTyped, *s.timeoutTyped = rec.Request.ModelTyped, rec.Request.TimeoutTyped
	*s.lead, *s.replayed = rec.Request.Lead, rec.Request // the routed lead is not a flag: the replay leads with it
	clearTyped(fs, s)
	if _, err := helptext.ParseArgs(fs, args); err != nil { // re-apply explicit flags over the record
		return ctx, failAskCode(format, *s.Name, err, 2), true
	}
	if err := retryWorkdir(fs, s.Workdir, *s.Host, rec); err != nil {
		return ctx, failAskCode(format, *s.Name, err, 2), true
	}
	*prompt, *s.rubric = cmp.Or(*prompt, rec.Request.Prompt), "" // a typed task replaces the logged one
	fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	moved := moveOffCutoff(fs, s, rec, os.Stderr, "")
	return agent.WithRetried(ctx, agent.Retried{ID: rec.ID, Plan: replayPlan(fs, rec, moved)}), 0, false
}

// replayPlan is rec's plan for its retry: a cut-off workspace never leads, a claude move is its runner (D5).
func replayPlan(fs *flag.FlagSet, rec agent.Record, moved bool) *agent.RoutePlan {
	if rec.RoutePlan == nil {
		return nil // a record from before route_plan replays as today
	}
	p := *rec.RoutePlan // PlanRoute reads only the replayed fields; slices are replaced, never mutated
	if rec.CutOff() {
		p.Pool, p.Admitted = nil, nil
	}
	if moved {
		p.Agent, p.Barred = agentid.Claude, "a cut-off retry runs claude's own CLI"
		p.Runners = []agent.Runner{{Kind: agent.RunnerOwn, Agent: agentid.Claude}}
	}
	if helptext.Explicit(fs, "runner") {
		p.Runners = nil
	}
	if helptext.Explicit(fs, "fallback") {
		p.Fallback = router.Policy{}
	}
	if helptext.Explicit(fs, "profile") || helptext.Explicit(fs, "provider") {
		p.Pool, p.Admitted, p.Fence = nil, nil, route.Fence{}
	}
	return &p
}

// retryWorkdir pins a retry to the directory the run used (its cwd, tree, else repo) and checks it exists.
func retryWorkdir(fs *flag.FlagSet, workdir *string, host string, rec agent.Record) error {
	if helptext.Explicit(fs, "workdir") || host != "" { // a remote path is not checkable here
		return nil
	}
	dir := cmp.Or(rec.Cwd, rec.Request.WorkDir, rec.Tree, rec.Repo) // Cwd is absolute; a logged --workdir may be relative
	if dir == "" {
		return nil
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("--retry %s: the logged directory %s no longer exists; pass --workdir <dir> on the retry", rec.ID, dir)
	}
	*workdir = dir
	return nil
}

// keepReason names why a retry stays on its cut-off route, "" when it may move.
func keepReason(typedModel, noFallback bool, ran string) string {
	switch {
	case typedModel:
		return "a typed model; drop --model or add --agent claude to move it"
	case noFallback:
		return "--fallback none; add --agent claude to move it"
	case ran == "claude":
		return "claude failed; add --agent <other> to move it"
	}
	return ""
}

// moveOffCutoff sends a cut-off run's retry to claude when nothing typed pins the route, else says how to move.
func moveOffCutoff(fs *flag.FlagSet, s *askState, rec agent.Record, errw io.Writer, ago string) (moved bool) {
	ran := cmp.Or(rec.RanAs, rec.Agent)
	if !rec.CutOff() || (ago == "" && helptext.Explicit(fs, "agent")) { // a resubmit always types --agent
		return false
	}
	auto := agent.IsAuto(*s.Model) // typed auto asks for rotation, not a pin
	typed, none := *s.modelTyped || helptext.Explicit(fs, "model") && !auto, *s.Fallback == router.FallbackNone
	id := rec.ID + ago
	if why := keepReason(typed, none, ran); why != "" {
		fmt.Fprintf(errw, "ask: %s cut off (%s) on %s; this retry keeps it (%s)\n", id, rec.Cutoff, ran, why)
		return false
	}
	*s.Name, *s.Model, *s.lead, *s.Coworker = "claude", "", "", ""
	force := "--agent " + ran + " forces the original"
	if ago != "" {
		force = "--model or --fallback none keeps the original"
	}
	fmt.Fprintf(errw, "ask: %s cut off (%s) on %s; retrying on claude (%s)\n", id, rec.Cutoff, ran, force)
	return true
}

// guardResubmit reads a plain ask's task early (stdin once) so a resubmit of a recent cutoff can leave its route.
func (r *askRun) guardResubmit() (int, bool) {
	f := r.f
	if f.retry != "" || f.job != "" || f.stdinFramed || f.live || r.persona != nil {
		return 0, false
	}
	prompt, err := resolveAskPrompt(r.prompt)
	if err != nil {
		return failAsk(f.format, f.name, err), true
	}
	r.prompt = prompt // stdin is read once; execute takes it from here
	guardResubmit(r.fs, r.state, prompt, os.Stderr, time.Now())
	return 0, false
}

// retryLineage rejoins the retried run's parent only within the caller's own tree, or top-level while it still runs.
func retryLineage(ctx context.Context, rec agent.Record) context.Context {
	if rec.Parent == "" {
		return ctx
	}
	_, values := agent.RunValues(ctx, nil)
	caller := values[proc.DispatchIDEnv]
	if caller == rec.ID { // agents send resumes a run as its own child
		return ctx
	}
	p, err := agent.LoadRecord(rec.Parent)
	if err != nil || (caller == "" && !p.Alive()) || (caller != "" && !inLineage(caller, rec.Parent)) {
		return ctx
	}
	// The parent's own depth, not the caller's, so the rerun lands at parent depth+1.
	depth := fmt.Sprint(p.Depth) //nolint:perfsprint // strconv would put the file over the 25-import cap
	return proc.WithValues(ctx, values.Merge(proc.Values{proc.DispatchIDEnv: rec.Parent, proc.DispatchDepthEnv: depth}))
}

// inLineage reports whether id is caller or one of its recorded ancestors.
func inLineage(caller, id string) bool {
	for n := 0; caller != "" && n < 64; n++ { // bounded so a corrupt parent cycle cannot spin
		if caller == id {
			return true
		}
		rec, err := agent.LoadRecord(caller)
		if err != nil {
			return false
		}
		caller = rec.Parent
	}
	return false
}

// guardResubmit applies moveOffCutoff to a plain ask whose agent, model and task cut off within the last hour.
func guardResubmit(fs *flag.FlagSet, s *askState, prompt string, errw io.Writer, now time.Time) {
	e, ok := agent.RecentCutoff(agent.CutoffKey(*s.Name, *s.Model, prompt), now)
	if !ok {
		return
	}
	moveOffCutoff(fs, s, agent.Record{ID: e.ID, Agent: e.Agent, Cutoff: e.Kind}, errw, " ("+now.Sub(e.Time).Truncate(time.Second).String()+" ago)")
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
		case "env":
			*s.env = nil
		}
	})
}

// loadJobRecord loads a --job record for a background run continuation and claims it for this process.
func loadJobRecord(id string) (agent.Record, error) { return agent.ClaimJob(id) }

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

// overlayLocal refuses an overlay's env layer or budget on --live or --host; routing keys and limits pass.
func overlayLocal(ov agentconfig.Overlay, live bool, host string) error {
	if !live && host == "" {
		return nil
	}
	ov.MaxChildren, ov.MaxTreeTokens = nil, nil // limits only bind nested dispatches, which a remote run still gets
	if ov.Empty() {
		return nil
	}
	var keys []string
	if len(ov.Set)+len(ov.Unset)+len(ov.Keep) > 0 {
		keys = append(keys, "set/unset/keep")
	}
	if ov.CodexNetwork {
		keys = append(keys, "codexNetwork")
	}
	if ov.Cloud {
		keys = append(keys, "cloud")
	}
	if ov.BudgetUSD != nil || ov.BudgetCredits != nil {
		keys = append(keys, "budgetUSD/budgetCredits")
	}
	return errors.New("overlay " + strings.Join(keys, "/") + " cannot reach --live or --host: run the agent locally")
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

// flagRoute (zero if none): --profile R takes R's provider, not the session's; --provider P: pool {P, auto} or its row.
func flagRoute(f *askFlags) route.Route {
	reg, err := providers.Load()
	if err != nil {
		reg = &providers.Registry{}
	}
	switch {
	case f.profile != "":
		if row := reg.Row(f.profile); row != nil {
			return route.Route{Provider: row.Provider, Profile: f.profile}
		}
		if r := route.ParseLegacy(f.profile); r.Provider != "" && r.Profile != "" {
			return r
		}
	case f.provider == router.RouteDatabricks:
		return route.Route{Provider: f.provider, Profile: route.AutoArm}
	case f.provider != "":
		if rows := reg.ProfilesOf(f.provider); len(rows) == 1 {
			return route.Route{Provider: f.provider, Profile: rows[0]}
		}
		return route.Route{Provider: f.provider}
	}
	return route.Route{}
}

// flagConflict refuses a typed flag setting a key of the --overlay on the same command line to another value.
func flagConflict(fs *flag.FlagSet, f *askFlags, fr route.Route, ov agentconfig.Overlay) error {
	or := ov.Route()
	name, val := "--profile", f.profile
	if f.profile == "" {
		name, val = "--provider", f.provider
	}
	for _, c := range []struct{ flag, val, key, flagKey, ov string }{
		{name, val, "provider", fr.Provider, or.Provider},
		{name, val, "profile", fr.Profile, or.Profile},
		{"--as", f.as, "identity", f.as, or.Identity},
	} {
		if c.flagKey != "" && c.ov != "" && c.flagKey != c.ov {
			return fmt.Errorf("%s %s conflicts with overlay %s %s", c.flag, c.val, c.key, c.ov)
		}
	}
	return settingConflict(fs, ov)
}

// settingConflict refuses a typed --model, --effort, --fallback, --host or --runner that differs from its overlay key.
func settingConflict(fs *flag.FlagSet, ov agentconfig.Overlay) error {
	for _, k := range [][2]string{{"model", ov.Model}, {"effort", ov.Effort}, {"fallback", ov.Fallback}, {"host", ov.Host}, {"runner", ov.Runner}} {
		if v := fs.Lookup(k[0]).Value.String(); helptext.Explicit(fs, k[0]) && v != "" && k[1] != "" && v != k[1] {
			return fmt.Errorf("--%s %s conflicts with overlay %s %s", k[0], v, k[0], k[1])
		}
	}
	return nil
}

// readPrompt reads the task; framed (stdin) is set only for the remote end of `ask --host`.
func (r *askRun) readPrompt() (*bufio.Reader, string, error) {
	if r.f.stdinFramed {
		return readFramedPrompt(os.Stdin)
	}
	prompt, err := resolveAskPrompt(r.prompt)
	return nil, agent.WithTier(prompt, r.f.tier), err
}

// recoverKey carries a *recoverClaim: the dispatch a --recover targets and whether this run holds its marker.
type recoverKey struct{}

type recoverClaim struct {
	from string
	held bool
}

func recoverOf(ctx context.Context) *recoverClaim {
	c, _ := ctx.Value(recoverKey{}).(*recoverClaim)
	return c
}

// claimRecover takes the one recovery claim on dispatch id (an O_EXCL marker); the loser learns the winner's id.
func claimRecover(ctx context.Context) error {
	c := recoverOf(ctx)
	if c == nil {
		return nil
	}
	rec, err := agent.LoadRecord(c.from)
	if err != nil {
		return err
	}
	if rec.Alive() {
		return checkResumable(rec)
	}
	marker := agent.WatchMarker("resumed", rec.ID)
	if marker == "" {
		return fmt.Errorf("dispatch id %q cannot be claimed", rec.ID)
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if id, _ := agent.ResumedAs(rec.ID); id != "" {
			return fmt.Errorf("dispatch %s already recovered as %s", rec.ID, id)
		}
		return fmt.Errorf("dispatch %s is already being recovered by another session", rec.ID)
	}
	if err != nil {
		return err
	}
	c.held = true
	return f.Close()
}

// finishRecover records the new dispatch id in the claim marker of a --recover run.
func finishRecover(ctx context.Context, id string) {
	if c := recoverOf(ctx); c != nil && c.held {
		if m := agent.WatchMarker("resumed", c.from); m != "" {
			_ = os.WriteFile(m, []byte(id+"\n"), 0o600)
		}
	}
}

// releaseRecover drops a --recover claim that never got a dispatch id, so a failed start can be retried.
func releaseRecover(ctx context.Context) {
	c := recoverOf(ctx)
	if c == nil || !c.held {
		return
	}
	if m := agent.WatchMarker("resumed", c.from); m != "" {
		if b, err := os.ReadFile(m); err == nil && len(strings.TrimSpace(string(b))) == 0 {
			_ = os.Remove(m)
		}
	}
}

// recoverRetry turns a --retry into a recovery: swap in RecoveryPrompt and start a fresh session; the claim is later.
func (r *askRun) recoverRetry() (int, bool) {
	f := r.f
	if f.retry == "" {
		return r.fail(errors.New("--recover needs --retry <id>"), 2)
	}
	rec, err := agent.LoadRecord(f.retry)
	if err != nil {
		return r.fail(err, 1)
	}
	if rec.Alive() {
		return r.fail(checkResumable(rec), 1)
	}
	r.ctx = context.WithValue(r.ctx, recoverKey{}, &recoverClaim{from: rec.ID})
	if !helptext.Explicit(r.fs, "session") {
		f.session = "" // never inherit the cut-off turn's session
	}
	if r.prompt != rec.Request.Prompt { // a typed task replaces the logged one: nothing to recover into
		return 0, false
	}
	p, err := agent.RecoveryPrompt(rec, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: --recover: %v; plain retry\n", err)
		return 0, false
	}
	r.prompt = p
	fmt.Fprintf(os.Stderr, "ask: recovering %s on a fresh session\n", rec.ID)
	return 0, false
}

// planTaskCheck refuses a --plan-task that is not a plan id, or on a run that dispatches more than one task.
func planTaskCheck(f *askFlags) error {
	switch {
	case f.planTask == "":
		return nil
	case !planTaskRE().MatchString(f.planTask):
		return fmt.Errorf("--plan-task %q must match %s", f.planTask, planTaskRE())
	case f.batchSrc != "" || f.live || f.loop || f.loopResume != "" || f.waves != "":
		return errors.New("--plan-task names one task: not with --batch, --live, --loop or --waves (a batch line carries plan_task)")
	case f.job != "":
		return errors.New("--plan-task stamps a new dispatch: not with --job (the record already carries it)")
	}
	return nil
}

// contractHostCheck refuses a spec contract on --host: its lane, gate and proxy terms are observed on this machine.
func contractHostCheck(f *askFlags) error {
	if f.contract != nil && f.host != "" {
		return errors.New("spec key contract cannot run with --host: its terms are observed on this machine")
	}
	return nil
}

// planTaskKey carries a single ask's --plan-task to askPrepare, which stamps it on the new dispatch record.
type planTaskKey struct{}

func withPlanTask(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, planTaskKey{}, id)
}

func planTaskOf(ctx context.Context) string {
	id, _ := ctx.Value(planTaskKey{}).(string)
	return id
}

// stampPlanTask puts ctx's --plan-task on a new dispatch record; a --job child's record already holds it.
func stampPlanTask(ctx context.Context, jobRec, rec *agent.Record) {
	if id := planTaskOf(ctx); id != "" && jobRec == nil {
		rec.PlanTask = id
	}
}
