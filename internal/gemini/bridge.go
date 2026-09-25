package gemini

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

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

	files, err := diffFiles(o)
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
	ctxFiles, err := CollectContext(o.Cwd, o.Dirs, files, maxFiles, o.MaxFileBytes)
	if err != nil {
		return agent.Result{}, err
	}
	if o.Index {
		task += "\n\n" + IndexPrompt(ctxFiles)
	}
	prompt := BuildPrompt(task, ctxFiles)
	logContextStats(log, ctxFiles)

	if o.PrintCommand {
		return printCommand(reg, sel, p, model, ctxFiles, prompt, log), nil
	}

	r, err := agent.Lookup("api")
	if err != nil {
		return agent.Result{}, err
	}
	return agent.Run(ctx, r, agent.Request{
		Prompt: prompt, Model: model, Provider: p.Name, Effort: o.Effort, Timeout: o.Timeout,
	}, stream, func(s string) { fmt.Fprintln(log, s) })
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
func diffFiles(o Options) ([]string, error) {
	files := append([]string{}, o.Files...)
	if !o.Diff {
		return files, nil
	}
	changed, err := ChangedFilesVsHEAD(o.Cwd)
	if err != nil {
		return nil, fmt.Errorf("--diff: %w", err)
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("--diff: nothing changed vs HEAD")
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
	shown := model // a slot name or provider:id resolves the way agent.Run will, so the line names the real id
	if t, ok := sel.Slots[model]; ok {
		shown = t.Model
	} else {
		shown = providers.ParseTarget(reg, model, p).Model
	}
	fmt.Fprintf(log, "POST %s%s model=%s provider=%s files=%d prompt_bytes=%d\n",
		route, suffix, shown, p.Name, len(ctxFiles.Included), len(prompt))
	return agent.Result{Provider: p.Name, Model: shown}
}
