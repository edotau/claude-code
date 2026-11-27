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
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/driver/googletools"
	"github.com/edotau/claude-code/internal/driver/pi"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
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
	brief, verify, action                                                                                                                                                              string
	actionOwn                                                                                                                                                                          []string
	contract                                                                                                                                                                           *contractOpen
	planTask                                                                                                                                                                           string
	dryRun                                                                                                                                                                             bool
	loop                                                                                                                                                                               bool
	waves                                                                                                                                                                              string
	loopPlanner, loopResume                                                                                                                                                            string
	loopRounds, loopBudget                                                                                                                                                             int
	cli                                                                                                                                                                                []string
}

// CompletionFlags lists ask's flags for tab completion; choice flags offer the schema's Choices lists.
func CompletionFlags() []helptext.Flag {
	fs, _ := askFlagSet()
	values := map[string]func() []string{"agent": engineNames, "profile": providers.ProfileNames}
	for _, f := range allFlagSchema().Fields {
		if len(f.Choices) > 0 {
			values[f.Name] = func() []string { return f.Choices }
		}
	}
	return helptext.FlagsOf(fs, values)
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
	fs.StringVar(&f.planTask, "plan-task", "", "plan task id this one ask implements: the record's plan_task and a brief's Task: line")
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
	fs.DurationVar(&f.timeout, "timeout", agent.DefaultToolsTimeout, "per-attempt deadline; a foreground ask past the 10m Bash cap runs --background")
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
	fs.StringVar(&f.brief, "brief", "", "the task as JSON (prompt, own, report, verify, contract, action): file, inline {...} or -")
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
	if err := enumFlag("codemode", mode, route.Codemodes); err != nil {
		return "", "", err
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
	if runner == route.RunnerCLI || direct || !agentid.Known(ag) || agentid.Routing(ag).CodeMode.Has() {
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

// Set accepts anything: the flag parser echoes a rejected value in its error, so parseAgentDefs validates it later.
func (a *agentsFlag) Set(s string) error {
	*a = agentsFlag(s)
	return nil
}

// validateAskFlags checks --format and --fallback against their allowed values.
func validateAskFlags(format, fallback string) error {
	if !slices.Contains(agent.Formats, format) {
		return fmt.Errorf("--format %q unknown (text|json|stream-json)", format)
	}
	if !router.ValidFallback(fallback) {
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
	tools, err := googletools.Parse(strings.Split(csv, ","))
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = string(t)
	}
	return out, err
}

// enumFlag shares overlay enum validation while preserving the CLI's diagnostic.
func enumFlag(name, value string, choices []string) error {
	if route.OneOf(name, value, choices) != nil {
		return fmt.Errorf("--%s %q must be %s", name, value, strings.Join(choices, " or "))
	}
	return nil
}

// runnerCheck refuses a --runner outside route.Runners.
func runnerCheck(f *askFlags) error {
	return enumFlag("runner", f.runner, route.Runners)
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

// askTimeout is the deadline: a fresh --tools run with neither --timeout nor an overlay timeout gets 15m.
func askTimeout(fs *flag.FlagSet, ov agentconfig.Overlay, timeout time.Duration, retry, job string, tools bool) time.Duration {
	if retry == "" && job == "" && tools && !timeoutHeld(fs, ov) {
		return agent.DefaultToolsTimeout
	}
	return timeout
}

// timeoutHeld reports a typed --timeout or an overlay timeout: the tools default never replaces it.
func timeoutHeld(fs *flag.FlagSet, ov agentconfig.Overlay) bool {
	return ov.Timeout != "" || helptext.Explicit(fs, "timeout")
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

// overlayLocal refuses an overlay's env layer, budget, codexNetwork or cloud on --live or --host; the rest pass.
func overlayLocal(ov agentconfig.Overlay, live bool, host string) error {
	if !live && host == "" {
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
	if len(keys) == 0 {
		return nil
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

const routeForms = "use --provider P --profile N or --overlay '{\"provider\":\"P\",\"profile\":\"N\"}'"

// checkTypedRoute refuses the colon form on every input, then a half-typed route (see checkRoutePair).
func checkTypedRoute(f *askFlags, envOverlay string) error {
	if strings.Contains(f.provider+f.profile, ":") || route.ColonForm(f.overlay) || route.ColonForm(envOverlay) {
		return errors.New("the provider:profile form is not accepted: " + routeForms)
	}
	if cmp.Or(f.overlay, envOverlay, f.host, f.job, f.retry, f.batchSrc) != "" || f.native {
		return nil
	}
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	return checkRoutePair(reg, f.provider, f.profile)
}

// checkRoutePair refuses a provider with profiles typed without one, and a registry profile typed without its provider.
func checkRoutePair(reg *providers.Registry, provider, profile string) error {
	switch {
	case provider != "" && profile == "" && len(reg.ProfilesOf(provider)) > 0:
		return fmt.Errorf("--provider %s needs --profile: %s", provider, routeForms)
	case provider == "" && profile != "" && (reg.Row(profile) != nil || strings.Contains(profile, "·")):
		return fmt.Errorf("--profile needs --provider: %s", routeForms)
	}
	return nil
}

// flagConflict refuses a typed flag setting a key of a batch line's overlay to another value.
func flagConflict(fs *flag.FlagSet, f *askFlags, fr route.Route, ov agentconfig.Overlay) error {
	return cmp.Or(unitConflict(f, fr, ov), settingConflict(fs, ov))
}

// unitConflict refuses a typed route flag naming another unit than the --overlay; a per-run flag beats its key instead.
func unitConflict(f *askFlags, fr route.Route, ov agentconfig.Overlay) error {
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
	return nil
}

// settingConflict refuses a typed --model, --effort, --fallback, --host or --runner that differs from its overlay key.
func settingConflict(fs *flag.FlagSet, ov agentconfig.Overlay) error {
	values := overlayFlags(ov)
	for _, key := range []string{"model", "effort", "fallback", "host", "runner"} {
		if v := fs.Lookup(key).Value.String(); helptext.Explicit(fs, key) && v != "" && values[key] != "" && v != values[key] {
			return fmt.Errorf("--%s %s conflicts with overlay %s %s", key, v, key, values[key])
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
		return errors.New("brief key contract cannot run with --host: its terms are observed on this machine")
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
