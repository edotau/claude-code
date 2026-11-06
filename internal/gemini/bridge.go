package gemini

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/pack"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// bridgeDispatchLabel marks a bridge run's dispatch Record so `ask --log` distinguishes it from an
// `ask --agent gemini` run, even though both execute on the same "gemini" Engine.
const bridgeDispatchLabel = "gemini-bridge"

// bridgeBudget bounds the whole rendered prompt (2.5 MiB, ~2.4 bytes/token worst case under a 1M window); agy cuts the tail.
const bridgeBudget = 2560 * 1024

// GeminiOptions configures one bridge run: inline workspace files into one prompt, one chat call.
type GeminiOptions struct {
	Task, Model, Provider, Effort, Rubric, Cwd string
	Dirs, Files                                []string
	MaxFiles, MaxFileBytes                     int // <=0 → package defaults
	Timeout                                    time.Duration
	Index, Diff, PrintCommand, IndexOnly       bool // IndexOnly inlines no bodies (the index still lists every path)
	Launcher                                   agent.Launcher
}

// bridgeTask applies the rubric, when set, to the task text.
func bridgeTask(o GeminiOptions) (string, error) {
	if o.Rubric == "" {
		return o.Task, nil
	}
	return agent.RubricPrompt(o.Rubric, o.Task)
}

// warnPrompt writes the agy-truncation warning for the fitted prompt to stream.
func warnPrompt(stream io.Writer, o GeminiOptions, prompt string) {
	if o.Provider == "" && len(prompt) > agyTruncateWarnBytes {
		fmt.Fprintf(stream, "bridge: warning: prompt is %d bytes; agy may silently truncate input past ~%d KB\n", len(prompt), agyTruncateWarnBytes/1024)
	}
}

// bridgePrompt builds the task (rubric applied), inlines the workspace context under bridgeBudget, and warns on stream.
func bridgePrompt(ctx context.Context, o GeminiOptions, stream, log io.Writer) (string, pack.Context, error) {
	task, err := bridgeTask(o)
	if err != nil {
		return "", pack.Context{}, err
	}
	spec := pack.Spec{Dirs: o.Dirs, Files: o.Files, Diff: o.Diff, Index: o.Index, MaxFiles: o.MaxFiles,
		MaxFileBytes: o.MaxFileBytes, IndexOnly: o.IndexOnly}
	packed, err := pack.Build(ctx, spec, task, o.Cwd, bridgeBudget)
	if err != nil {
		return "", pack.Context{}, err
	}
	logContextStats(log, packed.Files)
	for _, n := range packed.Notes {
		fmt.Fprintln(stream, n)
	}
	warnPrompt(stream, o, packed.Prompt)
	return packed.Prompt, packed.Files, nil
}

// requireAgy fails fast when the agy binary is missing or not signed in.
func requireAgy() error {
	if _, lookErr := paths.AgentBin("agy"); lookErr != nil {
		return fmt.Errorf("gemini bridge: %w", lookErr)
	}
	if !GeminiLoginPresent() {
		return errors.New("gemini bridge: agy is not signed in; run `agy`, or pass --provider")
	}
	return nil
}

// RunGemini resolves the provider/model, collects workspace context into one prompt, and dispatches the
// chat call through the shared api agent — no HTTP code lives here.
func RunGemini(ctx context.Context, o GeminiOptions, stream, log io.Writer) (agent.Result, error) {
	reg, sel, p, model, err := resolveProvider(o)
	if err != nil {
		return agent.Result{}, err
	}
	prompt, ctxFiles, err := bridgePrompt(ctx, o, stream, log)
	if err != nil {
		return agent.Result{}, err
	}

	useAgy := o.Provider == "" // the default runs on agy's Google sign-in; --provider picks an API-key provider
	if useAgy && !o.PrintCommand {
		if err := requireAgy(); err != nil {
			return agent.Result{}, err
		}
	}
	if o.PrintCommand {
		if useAgy {
			return printAgyCommand(reg, sel, p, model, o, ctxFiles, prompt, log), nil
		}
		return printCommand(reg, sel, p, model, ctxFiles, prompt, log), nil
	}
	if useAgy {
		fmt.Fprintln(log, "gemini bridge: agy (Google login)")
		return runAgyBridge(ctx, o, geminiAgyModel(reg, sel, p, o.Model), ctxFiles, prompt, stream, log)
	}
	return runAPIBridge(ctx, o, p, model, prompt, stream, log)
}

// runAPIBridge sends the prompt through the shared claude agent against an API-key provider.
func runAPIBridge(ctx context.Context, o GeminiOptions, p *providers.Provider, model, prompt string,
	stream, log io.Writer) (agent.Result, error) {
	r, err := agent.Lookup("claude")
	if err != nil {
		return agent.Result{}, err
	}
	if o.Launcher == nil {
		o.Launcher = agent.DefaultLauncher()
	}
	return agent.Run(ctx, r, agent.RunOptions{
		Prompt: prompt, Model: model, Provider: p.Name, Effort: o.Effort, Timeout: o.Timeout, NoTools: true, Launcher: o.Launcher,
	}, agent.NewTextSink(stream), func(s string) { fmt.Fprintln(log, s) })
}

// runAgyBridge runs the prompt on agy's own stdin-NDJSON path, recorded as a compact dispatch Record.
func runAgyBridge(ctx context.Context, o GeminiOptions, model string, ctxFiles pack.Context, prompt string,
	stream, log io.Writer) (agent.Result, error) {
	agyReq := agyRequest{Prompt: prompt, Model: model, Effort: o.Effort, Cwd: o.Cwd}
	// agy's own stdin-NDJSON path, not Run/cliEngine.Run: that agent's argv-prompt cap
	// (MaxPromptBytes) and provider-fallback resolution are both wrong for a multi-MB inlined
	// sweep over agy's own Google login. Do + Classify reuse the same retry/backoff shape ask gets;
	// the context byte budget (bridgeBudget), not a quota step-down, bounds this prompt's size.
	policy := agent.DefaultPolicy()
	if o.Timeout > 0 {
		policy.AttemptTimeout = o.Timeout
	}
	policy.Notify = func(s string) { fmt.Fprintln(log, s) }

	// The Record never carries the full inlined prompt (MiBs of source): a compact summary instead.
	rec := agent.ClaimRecord(bridgeDispatchLabel, agent.RunOptions{
		Prompt: compactRecordPrompt(o.Task, ctxFiles), Model: model, Effort: o.Effort, WorkDir: o.Cwd, Timeout: o.Timeout,
	}, time.Now())
	rec.Owner = inflight.SessionOwner()
	rec.Log = rec.LogPath()
	_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run

	cw := &countingWriter{w: stream}
	return agent.RunRecorded(ctx, &rec, bridgeDispatchLabel, agent.NewTextSink(cw),
		agent.RunRecordedOpts{Notify: func(s string) { fmt.Fprintln(log, s) }},
		func(ctx context.Context, sink agent.Sink) (agent.Result, error) {
			return agyAttempts(ctx, policy, agyReq, cw, sink)
		})
}

// agyAttempts runs agy under the retry policy; a retry never fires once output has streamed.
func agyAttempts(ctx context.Context, policy agent.Policy, req agyRequest, cw *countingWriter, sink agent.Sink) (agent.Result, error) {
	var res agent.Result
	aerr := agent.Do(ctx, policy, func(actx context.Context, _ int) *agent.Error {
		cw.n = 0
		out, err := runAgy(actx, req, sink)
		res = out
		return agent.Classify("gemini", err, cw.n > 0)
	})
	res.Provider, res.Model = cmp.Or(res.Provider, "agy"), cmp.Or(res.Model, req.Model)
	if aerr != nil {
		return res, aerr
	}
	return res, nil
}

// resolvedModelID resolves an alias name or provider:model to the concrete model id agy expects.
func resolvedModelID(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string) string {
	if t, ok := sel.Aliases[model]; ok {
		return t.Model
	}
	return providers.ParseTarget(reg, model, p).Model
}

// resolveProvider selects the profile (default: the google profile with a gemini login) and resolves the model to call.
func resolveProvider(o GeminiOptions) (*providers.Registry, providers.Selection, *providers.Provider, string, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, nil, "", err
	}
	providerName := o.Provider
	if providerName == "" {
		for _, n := range reg.ProfilesOf("google") {
			if reg.Providers[n].Auth.Login == "gemini" {
				providerName = n
				break
			}
		}
	}
	sel, err := providers.Select(reg, providerName)
	if err != nil {
		return nil, providers.Selection{}, nil, "", err
	}
	p := sel.Provider
	if p == nil {
		return nil, providers.Selection{}, nil, "", fmt.Errorf("gemini: provider %q not found", providerName)
	}
	model := ResolveModel(o.Model, modelEnvKeys, p.Models[models.Sonnet])
	return reg, sel, p, model, nil
}

// logContextStats writes the indexed/inlined summary line, followed by the inlined path list.
func logContextStats(log io.Writer, ctxFiles pack.Context) {
	fmt.Fprintf(log, "%d path(s) indexed, %d inlined; skipped: %s\n",
		pack.IndexPathCount(ctxFiles), len(ctxFiles.Included), strings.Join(pack.SortedSkipReasons(ctxFiles), ", "))
	for _, f := range ctxFiles.Included {
		fmt.Fprintf(log, "  %s\n", f.Path)
	}
}

// printCommand logs the resolved request line for --print-command and returns the Result it would send.
func printCommand(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string,
	ctxFiles pack.Context, prompt string, log io.Writer) agent.Result {
	route, ok := p.Route(providers.ProtocolAnthropic)
	suffix := "/v1/messages"
	if !ok {
		route, _ = p.Route(providers.ProtocolOpenAI)
		suffix = "/chat/completions"
	}
	shown := geminiAgyModel(reg, sel, p, model) // an alias name or provider:id resolves the way Run will
	fmt.Fprintf(log, "POST %s%s model=%s provider=%s files=%d prompt_bytes=%d\n",
		route, suffix, shown, p.Name, len(ctxFiles.Included), len(prompt))
	return agent.Result{Provider: p.Name, Model: shown}
}

// printAgyCommand logs the resolved agy argv for --print-command on the agy backend and returns its Result.
func printAgyCommand(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string,
	o GeminiOptions, ctxFiles pack.Context, prompt string, log io.Writer) agent.Result {
	shown := geminiAgyModel(reg, sel, p, o.Model)
	argv := []string{"agy", "--model", shown}
	if o.Effort != "" {
		argv = append(argv, "--effort", o.Effort)
	}
	argv = append(argv, "--input-format", "stream-json", "--output-format", "stream-json", "-p=")
	fmt.Fprintf(log, "%s (stdin stream-json) files=%d prompt_bytes=%d\n",
		strings.Join(argv, " "), len(ctxFiles.Included), len(prompt))
	return agent.Result{Provider: "agy", Model: shown}
}

// countingWriter tracks whether any bytes reached stream this attempt, so a retry never repeats output
// that already streamed to the caller (Classify's Progressed gate).
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// compactRecordPrompt is the dispatch Record's Prompt field: the task plus a short inlined-file summary,
// never the full swept source (that would bloat state/dispatch to MiBs per bridge run).
func compactRecordPrompt(task string, ctxFiles pack.Context) string {
	const recordCap = 4 << 10 // a few KB: task text plus a representative file list
	var totalBytes int
	paths := make([]string, len(ctxFiles.Included))
	for i, f := range ctxFiles.Included {
		totalBytes += f.Bytes
		paths[i] = f.Path
	}
	out := fmt.Sprintf("%s\n[bridge: %d files, %d KB inlined: %s]",
		task, len(paths), totalBytes/1024, strings.Join(paths, ", "))
	if len(out) > recordCap {
		out = out[:recordCap-len("…(truncated)")] + "…(truncated)"
	}
	return out
}

// geminiAgyModel is the named (flag or process env) model for agy, else the GEMINI_MODEL pin, else AgyDefaultModel.
func geminiAgyModel(reg *providers.Registry, sel providers.Selection, p *providers.Provider, named string) string {
	if model := ResolveModel(named, modelEnvKeys, ""); model != "" {
		return resolvedModelID(reg, sel, p, model)
	}
	t, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "gemini", Target: providers.Target{Provider: p, Model: AgyDefaultModel}})
	return t.Model
}

// agyTruncateWarnBytes is the stdin size past which agy has been observed to drop input silently (~130 KB, S5).
const agyTruncateWarnBytes = 130 * 1024

// modelEnvKeys are process env names consulted in order when no explicit model is passed; the vault pin comes after.
var modelEnvKeys = []string{"CLAUDE_CODE_GEMINI_MODEL", "GEMINI_MODEL"}

// ResolveModel picks a model: explicit arg → first set env in envKeys → def.
func ResolveModel(arg string, envKeys []string, def string) string {
	if arg != "" {
		return arg
	}
	for _, key := range envKeys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return def
}
