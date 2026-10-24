package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// bridgeDispatchLabel marks a bridge run's dispatch Record so `ask --log` distinguishes it from an
// `ask --agent gemini` run, even though both execute on the same "gemini" Runner.
const bridgeDispatchLabel = "gemini-bridge"

// Options configures one bridge run: inline workspace files into one prompt, one chat call.
type Options struct {
	Task, Model, Provider, Effort, Rubric, Cwd string
	Dirs, Files                                []string
	MaxFiles, MaxFileBytes                     int // <=0 → package defaults
	Timeout                                    time.Duration
	Index, Diff, PrintCommand, IndexOnly       bool // IndexOnly inlines no bodies (the index still lists every path)
}

// Run resolves the provider/model, collects workspace context into one prompt, and dispatches the
// chat call through the shared api runner — no HTTP code lives here.
func Run(ctx context.Context, o Options, stream, log io.Writer) (agent.Result, error) {
	reg, sel, p, model, err := resolveProvider(o)
	if err != nil {
		return agent.Result{}, err
	}

	files, err := diffFiles(ctx, o)
	if err != nil {
		return agent.Result{}, err
	}

	task := o.Task
	if o.Rubric != "" {
		task, err = agent.RubricPrompt(o.Rubric, task)
		if err != nil {
			return agent.Result{}, err
		}
	}

	maxFiles := o.MaxFiles
	if o.IndexOnly {
		maxFiles = 0
	} else if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	ctxFiles, err := CollectContext(ctx, o.Cwd, o.Dirs, files, maxFiles, o.MaxFileBytes)
	if err != nil {
		return agent.Result{}, err
	}
	if o.Index {
		task += "\n\n" + IndexPrompt(ctxFiles)
	}
	prompt := BuildPrompt(task, ctxFiles)
	logContextStats(log, ctxFiles)
	if w := CoverageWarning(ctxFiles); w != "" {
		fmt.Fprintln(stream, w)
	}

	useAgy := o.Provider == "" // the default runs on agy's Google sign-in; --provider picks an API-key provider
	if useAgy && !o.PrintCommand {
		if _, lookErr := paths.VendorBin("agy"); lookErr != nil {
			return agent.Result{}, fmt.Errorf("gemini bridge: %w", lookErr)
		}
		if !vendors.GeminiLoginPresent() {
			return agent.Result{}, errors.New("gemini bridge: agy is not signed in; run `agy`, or pass --provider")
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
		model := agyModel(reg, sel, p, o.Model)
		agyReq := agent.AgyRequest{Prompt: prompt, Model: model, Effort: o.Effort, Cwd: o.Cwd}
		// agy's own stdin-NDJSON path, not agent.Run/vendorRunner.Run: that runner's argv-prompt cap
		// (agent.MaxPromptBytes) and provider-fallback resolution are both wrong for a multi-MB inlined
		// sweep over agy's own Google login. Do + Classify reuse the same retry/backoff shape ask gets;
		// the context byte budget (DefaultTotalBytes), not a quota step-down, bounds this prompt's size.
		policy := agent.DefaultPolicy()
		if o.Timeout > 0 {
			policy.AttemptTimeout = o.Timeout
		}
		policy.Notify = func(s string) { fmt.Fprintln(log, s) }

		// The Record never carries the full inlined prompt (MiBs of source): a compact summary instead.
		rec := agent.NewRecord(bridgeDispatchLabel, agent.Request{
			Prompt: compactRecordPrompt(o.Task, ctxFiles), Model: model, Effort: o.Effort, WorkDir: o.Cwd, Timeout: o.Timeout,
		}, time.Now())
		rec.Log = rec.LogPath()
		sink := agent.NewLogSink(rec.LogPath())
		_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run

		start := time.Now()
		cw := &countingWriter{w: stream}
		ctx = agent.WithLog(ctx, sink) // agy's raw events and stderr reach the log through the ctx carrier
		var res agent.Result
		aerr := agent.Do(ctx, policy, func(actx context.Context, _ int) *agent.Error {
			cw.n = 0
			out, err := agent.RunAgy(actx, agyReq, cw)
			res = out
			return agent.Classify("gemini", err, cw.n > 0)
		})
		var err error
		if aerr != nil {
			err = aerr
		}
		sink.Close() // flush before the record leaves running, so tail -f never misses the last line
		rec.Finish(res, err, time.Since(start))
		_ = rec.Save()
		return res, err
	}

	r, err := agent.Lookup("api")
	if err != nil {
		return agent.Result{}, err
	}
	return agent.Run(ctx, r, agent.Request{
		Prompt: prompt, Model: model, Provider: p.Name, Effort: o.Effort, Timeout: o.Timeout,
	}, stream, func(s string) { fmt.Fprintln(log, s) })
}

// resolvedModelID resolves a slot name or provider:model to the concrete model id agy expects.
func resolvedModelID(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string) string {
	if t, ok := sel.Slots[model]; ok {
		return t.Model
	}
	return providers.ParseTarget(reg, model, p).Model
}

// resolveProvider selects the provider (default "gemini") and resolves the model to call.
func resolveProvider(o Options) (*providers.Registry, providers.Selection, *providers.Provider, string, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, nil, "", err
	}
	providerName := o.Provider
	if providerName == "" {
		providerName = "gemini"
	}
	sel, err := providers.Select(reg, providerName)
	if err != nil {
		return nil, providers.Selection{}, nil, "", err
	}
	p := sel.Provider
	if p == nil {
		return nil, providers.Selection{}, nil, "", fmt.Errorf("gemini: provider %q not found", providerName)
	}
	model := ResolveModel(o.Model, ModelEnvKeys, p.Models[models.Sonnet])
	return reg, sel, p, model, nil
}

// diffFiles returns o.Files, plus files changed vs HEAD when o.Diff is set; a git failure or an empty
// diff is a usage error, not a silent full-tree fallback.
func diffFiles(ctx context.Context, o Options) ([]string, error) {
	files := append([]string{}, o.Files...)
	if !o.Diff {
		return files, nil
	}
	changed, err := ChangedFilesVsHEAD(ctx, o.Cwd)
	if err != nil {
		return nil, fmt.Errorf("--diff: %w", err)
	}
	if len(changed) == 0 {
		return nil, errors.New("--diff: nothing changed vs HEAD")
	}
	return append(files, changed...), nil
}

// logContextStats writes the indexed/inlined summary line, followed by the inlined path list.
func logContextStats(log io.Writer, ctxFiles Context) {
	fmt.Fprintf(log, "%d path(s) indexed, %d inlined; skipped: %s\n",
		IndexPathCount(ctxFiles), len(ctxFiles.Included), strings.Join(SortedSkipReasons(ctxFiles), ", "))
	for _, f := range ctxFiles.Included {
		fmt.Fprintf(log, "  %s\n", f.Path)
	}
}

// printCommand logs the resolved request line for --print-command and returns the Result it would send.
func printCommand(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string,
	ctxFiles Context, prompt string, log io.Writer) agent.Result {
	route, ok := p.Route(providers.DialectAnthropic)
	suffix := "/v1/messages"
	if !ok {
		route, _ = p.Route(providers.DialectOpenAI)
		suffix = "/chat/completions"
	}
	shown := agyModel(reg, sel, p, model) // a slot name or provider:id resolves the way agent.Run will
	fmt.Fprintf(log, "POST %s%s model=%s provider=%s files=%d prompt_bytes=%d\n",
		route, suffix, shown, p.Name, len(ctxFiles.Included), len(prompt))
	return agent.Result{Provider: p.Name, Model: shown}
}

// printAgyCommand logs the resolved agy argv for --print-command on the agy backend and returns its Result.
func printAgyCommand(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string,
	o Options, ctxFiles Context, prompt string, log io.Writer) agent.Result {
	shown := agyModel(reg, sel, p, o.Model)
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
// that already streamed to the caller (agent.Classify's Progressed gate).
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
func compactRecordPrompt(task string, ctxFiles Context) string {
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

// agyModel is the named (flag or env) model resolved for agy, or agent.AgyDefaultModel when none was named.
func agyModel(reg *providers.Registry, sel providers.Selection, p *providers.Provider, named string) string {
	model := ResolveModel(named, ModelEnvKeys, "")
	if model == "" {
		return agent.AgyDefaultModel
	}
	return resolvedModelID(reg, sel, p, model)
}
