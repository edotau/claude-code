package ask

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/network/remote"

	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
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
	keepEnv                                                                                                                                                                                                                       keepEnvFlag
	permAllow, permAsk, permDeny                                                                                                                                                                                                  ruleFlag
	permMode                                                                                                                                                                                                                      string
	leadProvider                                                                                                                                                                                                                  string
	env                                                                                                                                                                                                                           envFlag
}

func askFlagSet() (*flag.FlagSet, *askFlags) {
	fs := helptext.NewFlagSet("ask", askPage)
	f := &askFlags{}
	fs.StringVar(&f.name, "agent", "claude", "vendor, agents/<name>.md, vendor:name, or an --agents key")
	fs.Var(&f.agents, "agents", "agent definitions: --agent takes their keys")
	fs.StringVar(&f.fallback, "fallback", router.FallbackAll, "fallback: all (agents, then claude)|agents|none")
	fs.StringVar(&f.route, "route", "", "fallback ladder: default|first-party (default: by session profile)")
	fs.BoolVar(&f.native, "native", false, "opencode: skip the hub ladder; run on its default login")
	fs.StringVar(&f.provider, "profile", "", "profile, or session; leads, then the ladder (--fallback none pins)")
	fs.StringVar(&f.leadProvider, "provider", "", "lead on this provider's signed-in profile; falls back like --fallback")
	fs.StringVar(&f.as, "as", "", "claude, SP-paired Databricks profile: auto|user|sp (default sp)")
	fs.StringVar(&f.model, "model", "", "alias (opus), version (opus-5-5) or wire id")
	fs.StringVar(&f.effort, "effort", "", "reasoning effort: "+strings.Join(agentid.EffortNames(), "|"))
	fs.StringVar(&f.workdir, "workdir", "", "working directory for the agent")
	fs.StringVar(&f.session, "session", "", "resume this session id")
	fs.BoolVar(&f.tools, "tools", false, "allow tool use where the agent can gate it (default: disabled)")
	fs.BoolVar(&f.readOnly, "read-only", false, "tools may read files but never write, run shell or commit; implies --tools")
	fs.Var(&f.permAllow, "allow", "allow a tool call, e.g. 'Bash(ls:*)' (repeatable)")
	fs.Var(&f.permAsk, "ask", "ask before a tool call matching the rule (repeatable)")
	fs.Var(&f.permDeny, "deny", "deny a tool call matching the rule (repeatable)")
	fs.StringVar(&f.permMode, "mode", "", "permission mode for unmatched calls: default|auto|plan")
	fs.BoolVar(&f.noTools, "no-tools", false, "claude only: answer from the profile directly, no tools; not with --tools")
	fs.BoolVar(&f.noPersist, "no-persist", false, "claude, codex: do not save the session for later resume")
	fs.BoolVar(&f.safeMode, "safe-mode", false, "claude, codex, opencode: skip hooks, plugins, customizations")
	fs.StringVar(&f.debugFile, "debug-file", "", "claude, gemini: write the agent's debug log to this path")
	fs.Var(&f.attach, "attach", "attach a file (repeatable); task text precedes every attachment")
	fs.Var(&f.keepEnv, "keep-env", "keep NAME[,NAME] from the credential scrub (repeatable)")
	fs.Var(&f.env, "env", "set NAME=VALUE in the agent's environment (repeatable); credential names are refused")
	askRunFlags(fs, f)
	return fs, f
}

func askRunFlags(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.systemPrompt, "system-prompt", "", "claude only: replace the system prompt")
	fs.StringVar(&f.appendSystemPrompt, "append-system-prompt", "", "claude only: append to the system prompt")
	fs.StringVar(&f.outputSchema, "output-schema", "", "JSON Schema (file or inline) the answer must satisfy; envelope gains structured")
	fs.BoolVar(&f.direct, "direct", false, "agents: hand the CLI the profile route, skip the proxy")
	fs.BoolVar(&f.autonomous, "autonomous", false, "drive the run without stopping to ask, per agent")
	fs.BoolVar(&f.fleet, "fleet", false, "copilot only: run the task in Copilot's fleet mode (parallel subagents)")
	fs.BoolVar(&f.enableMemory, "enable-memory", false, "copilot only: turn on session memory")
	fs.DurationVar(&f.timeout, "timeout", 10*time.Minute, "per-attempt deadline (45m default with --tools)")
	fs.BoolVar(&f.stdinFramed, "stdin-framed", false, "internal: prompt is stdin up to a NUL byte; the run is cancelled when stdin then closes")
	fs.StringVar(&f.host, "host", "", "run the ask on this ssh host (tailnet name or ssh alias); profiles resolve there")
	fs.StringVar(&f.remoteBin, "remote-bin", "", "remote claude-code path for --host (default $"+remote.BinEnv+", then claude-code)")
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
	fs.IntVar(&f.jobs, "jobs", DefaultJobs(), "batch: tasks run at once")
	fs.BoolVar(&f.failFast, "fail-fast", false, "batch: the first failed task cancels the rest")
	fs.BoolVar(&f.follow, "follow", false, "batch: claim appended lines; {\"end\":true} ends")
	fs.DurationVar(&f.followIdle, "follow-idle", 30*time.Second, "batch: --follow idle limit")
	fs.StringVar(&f.retryAgent, "retry-agent", "", "batch: retry a failure once on these agents")
	fs.StringVar(&f.overlayPath, "overlay", "", "layer a JSON {set,unset,keep} file over the agent's env")
	fs.StringVar(&f.overlayDebug, "overlay-debug", "", "record each agent child's env names + salted fingerprints (never values) to <file>")
}

// askCheckFlags runs the flag checks needing no resolved agent; it returns the exit code with the error.
func askCheckFlags(env envFlag, keepEnv keepEnvFlag, format, fallback string, background bool, job string, tools, noTools bool) (int, error) {
	if _, err := agentconfig.FlagOverlay(env, keepEnv); err != nil { // names only: the error never carries a value
		return 2, err
	}
	if err := client.ValidateAskFlags(format, fallback); err != nil {
		return 1, err
	}
	return 1, client.AskExclusive(background, job, tools, noTools)
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
		if err := askLiveCombos(fs, name, hasPersona); err != nil {
			return err
		}
	}
	return askHostEnvCombos(host, background, job, batchSrc, hasEnv)
}

// askLiveCombos rejects --live with a persona, then checks the flags --live cannot take.
func askLiveCombos(fs *flag.FlagSet, name string, hasPersona bool) error {
	if hasPersona {
		return errors.New("--live cannot run a persona (agents/<name>.md or an --agents key): use a plain vendor")
	}
	return askLiveCheck(fs, name)
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
