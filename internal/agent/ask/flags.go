package ask

import (
	"bufio"
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
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// askFlags are the values of ask's flags.
type askFlags struct {
	name, fallback, route, provider, as, model, effort, workdir, session, debugFile, systemPrompt, appendSystemPrompt, outputSchema, host, remoteBin, format, rubric, retry, job, batchSrc, retryAgent, overlayPath, overlayDebug string
	native, tools, readOnly, noTools, noPersist, safeMode, direct, autonomous, fleet, enableMemory, stdinFramed, live, showLog, background, failFast, follow                                                                      bool
	timeout, followIdle                                                                                                                                                                                                           time.Duration
	jobs, maxFiles, maxFileBytes                                                                                                                                                                                                  int
	dirs, files                                                                                                                                                                                                                   string
	diff, index                                                                                                                                                                                                                   bool
	pack                                                                                                                                                                                                                          *pack.Spec
	agents                                                                                                                                                                                                                        agentsFlag
	attach                                                                                                                                                                                                                        attachFlag
	keepEnv                                                                                                                                                                                                                       []string
	permAllow, permAsk, permDeny                                                                                                                                                                                                  []string
	permMode                                                                                                                                                                                                                      string
	leadProvider                                                                                                                                                                                                                  string
	env                                                                                                                                                                                                                           []string
}

func askFlagSet() (*flag.FlagSet, *askFlags) {
	fs := helptext.NewFlagSet("ask", askPage)
	f := &askFlags{}
	fs.StringVar(&f.name, "agent", "claude", "agent, agents/<name>.md, <agent>:<name>, or an --agents key")
	fs.Var(&f.agents, "agents", "agent definitions: --agent takes their keys")
	fs.StringVar(&f.fallback, "fallback", "", "all|agents|none; all if "+router.EnvFallback+" set")
	fs.StringVar(&f.route, "route", "", "accounts: default|first-party|databricks[:<profile>|:auto] (bare: the session's Databricks profile; auto: the least busy workspace passing the preflight)")
	fs.BoolVar(&f.native, "native", false, "opencode: skip the hub ladder; run on its default login")
	fs.StringVar(&f.provider, "profile", "", "profile, or session; leads, then the pin (--fallback none pins)")
	fs.StringVar(&f.leadProvider, "provider", "", "lead on this provider's signed-in profile; falls back like --fallback")
	fs.StringVar(&f.as, "as", "", "claude only: auto|user|sp (default: the profile's)")
	fs.StringVar(&f.model, "model", "", "alias (opus), version (opus-5-5) or wire id")
	fs.StringVar(&f.effort, "effort", "", "reasoning effort: "+strings.Join(agentid.EffortNames(), "|"))
	fs.StringVar(&f.workdir, "workdir", "", "working directory for the agent")
	fs.StringVar(&f.session, "session", "", "resume this session id")
	fs.BoolVar(&f.tools, "tools", false, "allow tool use where the agent can gate it (default: disabled)")
	fs.BoolVar(&f.readOnly, "read-only", false, "read-only tools (claude, codex: tools off)")
	helptext.ListVar(fs, &f.permAllow, "allow", "RULE", "allow a tool call, e.g. 'Bash(ls:*)' (repeatable)", false)
	helptext.ListVar(fs, &f.permAsk, "ask", "RULE", "ask before a tool call matching the rule (repeatable)", false)
	helptext.ListVar(fs, &f.permDeny, "deny", "RULE", "deny a tool call matching the rule (repeatable)", false)
	fs.StringVar(&f.permMode, "mode", "", "permission mode for unmatched calls: default|auto|plan")
	fs.BoolVar(&f.noTools, "no-tools", false, "claude only: answer from the profile directly, no tools; not with --tools")
	fs.BoolVar(&f.noPersist, "no-persist", false, "claude, codex: do not save the session for later resume")
	fs.BoolVar(&f.safeMode, "safe-mode", false, "claude, codex, opencode: skip hooks, plugins, customizations")
	fs.StringVar(&f.debugFile, "debug-file", "", "claude, gemini: write the agent's debug log to this path")
	fs.Var(&f.attach, "attach", "attach a file (repeatable); task text precedes every attachment")
	helptext.ListVar(fs, &f.keepEnv, "keep-env", "NAME,NAME", "keep NAME[,NAME] from the credential scrub (repeatable)", true)
	helptext.ListVar(fs, &f.env, "env", "NAME=VALUE", "set NAME=VALUE in the agent's environment (repeatable); credential names are refused", false)
	askRunFlags(fs, f)
	return fs, f
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
	fs.StringVar(&f.host, "host", "", "run the ask on this ssh host (tailnet name, ssh alias, or auto: $"+remote.HostEnv+", else the one other tailnet machine); profiles resolve there, --route rides along")
	fs.StringVar(&f.remoteBin, "host-bin", "", "remote claude-code path for --host (default $"+remote.BinEnv+", then claude-code)")
	fs.StringVar(&f.format, "format", "text", "stream text, print the JSON result envelope, or stream-json NDJSON events")
	fs.StringVar(&f.dirs, "dirs", "", "comma-separated dirs whose tracked files are inlined (never .env*, state/, keys)")
	fs.StringVar(&f.files, "files", "", "comma-separated file globs to inline")
	fs.BoolVar(&f.diff, "diff", false, "inline the files changed vs HEAD")
	fs.BoolVar(&f.index, "index", false, "prepend an index of every matched path")
	fs.IntVar(&f.maxFiles, "max-files", pack.DefaultMaxFiles, "max files to inline (0 = index only)")
	fs.IntVar(&f.maxFileBytes, "max-file-bytes", pack.DefaultMaxFileBytes, "max bytes per inlined file")
	fs.StringVar(&f.rubric, "rubric", "", "review standard file quoted verbatim ahead of the task")
	fs.StringVar(&f.retry, "retry", "", "replay a logged dispatch by id (or unique id prefix); see --log")
	fs.BoolVar(&f.live, "live", false, "claude|gemini: hold a session open for agents send; --timeout is its idle limit")
	fs.BoolVar(&f.showLog, "log", false, "list the 20 most recent dispatches and exit")
	fs.BoolVar(&f.background, "background", false, "detach: log a job, print its id, exit")
	fs.StringVar(&f.job, "job", "", "internal: run saved dispatch <id> in place")
	fs.StringVar(&f.batchSrc, "batch", "", "run JSONL tasks from <file> or - on a pool; one envelope per line")
	fs.IntVar(&f.jobs, "jobs", defaultJobs(), "batch: tasks run at once")
	fs.BoolVar(&f.failFast, "fail-fast", false, "batch: the first failed task cancels the rest")
	fs.BoolVar(&f.follow, "follow", false, "batch: claim appended lines; {\"end\":true} ends")
	fs.DurationVar(&f.followIdle, "follow-idle", 30*time.Second, "batch: --follow idle limit")
	fs.StringVar(&f.retryAgent, "retry-agent", "", "batch: retry a failure once on these agents")
	fs.StringVar(&f.overlayPath, "overlay", "", "layer a JSON {set,unset,keep} file over the agent's env")
	fs.StringVar(&f.overlayDebug, "overlay-debug", "", "record each agent child's env names + salted fingerprints (never values) to <file>")
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

// askPack builds the pack spec from --dirs/--files/--diff; nil when none is set. The tuning flags need one.
func askPack(fs *flag.FlagSet, f *askFlags) (*pack.Spec, error) {
	if f.dirs == "" && f.files == "" && !f.diff {
		for _, n := range []string{"index", "max-files", "max-file-bytes"} {
			if helptext.Explicit(fs, n) {
				return nil, fmt.Errorf("--%s needs --dirs, --files or --diff", n)
			}
		}
		return nil, nil
	}
	if f.maxFiles < 0 || f.maxFileBytes < 0 {
		return nil, errors.New("--max-files and --max-file-bytes cannot be negative")
	}
	if f.batchSrc != "" || f.live || f.host != "" {
		return nil, errors.New("--dirs/--files/--diff cannot be combined with --batch, --live or --host")
	}
	return &pack.Spec{Dirs: textutil.SplitCSV(f.dirs), Files: textutil.SplitCSV(f.files), Diff: f.diff, Index: f.index,
		MaxFiles: f.maxFiles, MaxFileBytes: f.maxFileBytes, IndexOnly: f.maxFiles == 0}, nil
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
	// reject non-image binary up front: it fails the same way on every agent, so it must never reach the fallback chain
	if data, err := os.ReadFile(abs); err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	} else if !agent.IsText(data) && !agent.IsImage(data) {
		return fmt.Errorf("--attach %q: binary file; attach text only", s)
	}
	*a = append(*a, abs)
	return nil
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

// askPositional is the task words; on resume the first positional sets session.
func askPositional(positional []string, resume bool, session *string) ([]string, error) {
	if !resume {
		return positional, nil
	}
	if len(positional) < 1 {
		return nil, errors.New("resume needs a session id: <agent> resume [flags] <id> <task>")
	}
	*session = positional[0]
	return positional[1:], nil
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

// readFramedPrompt reads the prompt up to the NUL that ends it and returns the reader, whose remaining EOF is the cancel signal.
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

// resolveOutputSchema reads --output-schema as inline JSON when it starts with "{", else as a file, and checks it parses.
func resolveOutputSchema(arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	body := strings.TrimSpace(arg)
	if !strings.HasPrefix(body, "{") {
		data, err := os.ReadFile(arg)
		if err != nil {
			return "", fmt.Errorf("--output-schema: %w", err)
		}
		body = strings.TrimSpace(string(data))
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return "", fmt.Errorf("--output-schema: not a JSON object: %w", err)
	}
	return body, nil
}

// askTarget bundles the ask flag pointers that a logged dispatch's request overwrites on --job/--retry.
type askTarget struct {
	Name, Provider, Model, Effort, Workdir, Session                   *string
	Tools, ReadOnly, NoTools, Autonomous, Fleet, EnableMemory, Direct *bool
	NoPersist, SafeMode, Native                                       *bool
	DebugFile, SystemPrompt, AppendSystemPrompt, OutputSchema         *string
	Host, RemoteBin, Fallback                                         *string
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
	*t.Host, *t.RemoteBin = rec.Host, rec.RemoteBin
	p := rec.Request.Permissions
	*t.PermMode, *t.PermAllow, *t.PermAsk, *t.PermDeny = p.Mode, agent.RuleStrings(p.Allow), agent.RuleStrings(p.Ask), agent.RuleStrings(p.Deny)
	if t.Native != nil {
		*t.Native = rec.Request.Native
	}
	if t.Pack != nil {
		*t.Pack = rec.Request.Pack
	}
	if t.Persona != nil {
		*t.Persona = rec.Request.Persona
	}
	if rec.FallbackPolicy != "" { // records from before the field keep this run's default
		*t.Fallback = rec.FallbackPolicy
	}
	if len(rec.Request.EnvNames) > 0 {
		fmt.Fprintf(errw, "ask: the logged run set env %s; values are not stored, so pass --env or --overlay again\n", strings.Join(rec.Request.EnvNames, ","))
	}
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

// resolveOverlay checks the --env/--keep-env shorthand and resolves the file overlay's vault refs, failing closed; a
// --background parent leaves the file to its --job child, which inherits $COWORKER_OVERLAY and resolves it itself.
func resolveOverlay(file agentconfig.Overlay, env, keep []string, background bool) (agentconfig.Resolved, error) {
	if _, err := agentconfig.FlagOverlay(env, keep); err != nil {
		return agentconfig.Resolved{}, err
	}
	if background {
		return agentconfig.Resolved{}, nil
	}
	return agentconfig.ResolveOverlay(file)
}

// overlayLocal refuses a file overlay on --live or --host: neither carries it, and --host would expose it to ps.
func overlayLocal(file agentconfig.Overlay, live bool, host string) error {
	if !file.Empty() && (live || host != "") {
		return errors.New("an overlay cannot reach --live or --host: run the agent locally")
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
