package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc/inflight"
	"github.com/edotau/claude-code/internal/providers"
)

// bridgeDispatchLabel marks a bridge run's dispatch Record so `ask --log` distinguishes it from an
// `ask --agent gemini` run, even though both execute on the same "gemini" Runner.
const bridgeDispatchLabel = "gemini-bridge"

// GeminiOptions configures one bridge run: inline workspace files into one prompt, one chat call.
type GeminiOptions struct {
	Task, Model, Provider, Effort, Rubric, Cwd string
	Dirs, Files                                []string
	MaxFiles, MaxFileBytes                     int // <=0 → package defaults
	Timeout                                    time.Duration
	Index, Diff, PrintCommand, IndexOnly       bool // IndexOnly inlines no bodies (the index still lists every path)
	Launcher                                   Launcher
}

// RunGemini resolves the provider/model, collects workspace context into one prompt, and dispatches the
// chat call through the shared api runner — no HTTP code lives here.
func RunGemini(ctx context.Context, o GeminiOptions, stream, log io.Writer) (Result, error) {
	reg, sel, p, model, err := resolveProvider(o)
	if err != nil {
		return Result{}, err
	}

	files, err := diffFiles(ctx, o)
	if err != nil {
		return Result{}, err
	}

	task := o.Task
	if o.Rubric != "" {
		task, err = RubricPrompt(o.Rubric, task)
		if err != nil {
			return Result{}, err
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
		return Result{}, err
	}
	prompt, ctxFiles, dropped, err := FitPrompt(func(c Context) string {
		if o.Index {
			return task + "\n\n" + IndexPrompt(c)
		}
		return task
	}, ctxFiles, PromptCap)
	if err != nil {
		return Result{}, err
	}
	logContextStats(log, ctxFiles)
	if w := CoverageWarning(ctxFiles); w != "" {
		fmt.Fprintln(stream, w)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(stream, "bridge: prompt over %d KB; left out whole files: %s\n", PromptCap/1024, strings.Join(dropped, ", "))
	}

	useAgy := o.Provider == "" // the default runs on agy's Google sign-in; --provider picks an API-key provider
	if useAgy && !o.PrintCommand {
		if _, lookErr := paths.VendorBin("agy"); lookErr != nil {
			return Result{}, fmt.Errorf("gemini bridge: %w", lookErr)
		}
		if !GeminiLoginPresent() {
			return Result{}, errors.New("gemini bridge: agy is not signed in; run `agy`, or pass --provider")
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
		model := geminiAgyModel(reg, sel, p, o.Model)
		agyReq := agyRequest{Prompt: prompt, Model: model, Effort: o.Effort, Cwd: o.Cwd}
		// agy's own stdin-NDJSON path, not Run/cliRunner.Run: that runner's argv-prompt cap
		// (MaxPromptBytes) and provider-fallback resolution are both wrong for a multi-MB inlined
		// sweep over agy's own Google login. Do + Classify reuse the same retry/backoff shape ask gets;
		// the context byte budget (DefaultTotalBytes), not a quota step-down, bounds this prompt's size.
		policy := DefaultPolicy()
		if o.Timeout > 0 {
			policy.AttemptTimeout = o.Timeout
		}
		policy.Notify = func(s string) { fmt.Fprintln(log, s) }

		// The Record never carries the full inlined prompt (MiBs of source): a compact summary instead.
		rec := ClaimRecord(bridgeDispatchLabel, RunOptions{
			Prompt: compactRecordPrompt(o.Task, ctxFiles), Model: model, Effort: o.Effort, WorkDir: o.Cwd, Timeout: o.Timeout,
		}, time.Now())
		rec.Owner = inflight.SessionOwner()
		rec.Log = rec.LogPath()
		_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run

		cw := &countingWriter{w: stream}
		return Dispatch(ctx, &rec, bridgeDispatchLabel, NewTextSink(cw),
			DispatchOpts{Notify: func(s string) { fmt.Fprintln(log, s) }},
			func(ctx context.Context, sink Sink) (Result, error) {
				var res Result
				aerr := Do(ctx, policy, func(actx context.Context, _ int) *Error {
					cw.n = 0
					out, err := runAgy(actx, agyReq, sink)
					res = out
					return Classify("gemini", err, cw.n > 0)
				})
				res.Provider, res.Model = cmp.Or(res.Provider, "agy"), cmp.Or(res.Model, model)
				if aerr != nil {
					return res, aerr
				}
				return res, nil
			})
	}

	r, err := Lookup("claude")
	if err != nil {
		return Result{}, err
	}
	if o.Launcher == nil {
		o.Launcher = claudeLauncher{}
	}
	return Run(ctx, r, RunOptions{
		Prompt: prompt, Model: model, Provider: p.Name, Effort: o.Effort, Timeout: o.Timeout, NoTools: true, Launcher: o.Launcher,
	}, NewTextSink(stream), func(s string) { fmt.Fprintln(log, s) })
}

// resolvedModelID resolves a slot name or provider:model to the concrete model id agy expects.
func resolvedModelID(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string) string {
	if t, ok := sel.Slots[model]; ok {
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

// diffFiles returns o.Files, plus files changed vs HEAD when o.Diff is set; a git failure or an empty
// diff is a usage error, not a silent full-tree fallback.
func diffFiles(ctx context.Context, o GeminiOptions) ([]string, error) {
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
	ctxFiles Context, prompt string, log io.Writer) Result {
	route, ok := p.Route(providers.ProtocolAnthropic)
	suffix := "/v1/messages"
	if !ok {
		route, _ = p.Route(providers.ProtocolOpenAI)
		suffix = "/chat/completions"
	}
	shown := geminiAgyModel(reg, sel, p, model) // a slot name or provider:id resolves the way Run will
	fmt.Fprintf(log, "POST %s%s model=%s provider=%s files=%d prompt_bytes=%d\n",
		route, suffix, shown, p.Name, len(ctxFiles.Included), len(prompt))
	return Result{Provider: p.Name, Model: shown}
}

// printAgyCommand logs the resolved agy argv for --print-command on the agy backend and returns its Result.
func printAgyCommand(reg *providers.Registry, sel providers.Selection, p *providers.Provider, model string,
	o GeminiOptions, ctxFiles Context, prompt string, log io.Writer) Result {
	shown := geminiAgyModel(reg, sel, p, o.Model)
	argv := []string{"agy", "--model", shown}
	if o.Effort != "" {
		argv = append(argv, "--effort", o.Effort)
	}
	argv = append(argv, "--input-format", "stream-json", "--output-format", "stream-json", "-p=")
	fmt.Fprintf(log, "%s (stdin stream-json) files=%d prompt_bytes=%d\n",
		strings.Join(argv, " "), len(ctxFiles.Included), len(prompt))
	return Result{Provider: "agy", Model: shown}
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

// geminiAgyModel is the named (flag or process env) model for agy, else the GEMINI_MODEL pin, else AgyDefaultModel.
func geminiAgyModel(reg *providers.Registry, sel providers.Selection, p *providers.Provider, named string) string {
	if model := ResolveModel(named, modelEnvKeys, ""); model != "" {
		return resolvedModelID(reg, sel, p, model)
	}
	t, _ := ResolveAgentModel(ModelAsk{Agent: "gemini", Target: providers.Target{Provider: p, Model: AgyDefaultModel}})
	return t.Model
}

// ChangedFilesVsHEAD lists tracked files changed against HEAD (staged + unstaged + untracked), which is
// what a review run is actually about — reviewing the whole tree buries the diff. An error from either
// git command (not found, no HEAD, not a repo, …) is returned with its stderr instead of being masked.
func ChangedFilesVsHEAD(ctx context.Context, root string) ([]string, error) {
	var all []string
	for _, args := range [][]string{
		// --relative matches ls-files's cwd-relative output, so a subdirectory run doesn't mix bases.
		{"-C", root, "diff", "--name-only", "--relative", "HEAD"},
		{"-C", root, "ls-files", "--others", "--exclude-standard"},
	} {
		out, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			stderr := stderrOf(err)
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
		}
		all = append(all, strings.Split(strings.TrimSpace(string(out)), "\n")...)
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range all {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq, nil
}

// stderrOf extracts a *exec.ExitError's trimmed stderr, or empty for any other error type.
func stderrOf(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}

// PromptCap bounds the whole rendered prompt (task, inventory, tags), not just file bodies; over it agy cuts the tail.
const PromptCap = DefaultTotalBytes

// FitPrompt renders the prompt, moving whole trailing files to Skipped ("prompt-cap") until it fits limit.
// task is rebuilt per trim so an index reflects what stays inlined; it errors if zero files still exceed limit.
func FitPrompt(task func(Context) string, ctx Context, limit int) (string, Context, []string, error) {
	var dropped []string
	for {
		prompt := BuildPrompt(task(ctx), ctx)
		excess := len(prompt) - limit
		if excess > 0 && len(ctx.Included) == 0 {
			return prompt, ctx, dropped, fmt.Errorf("gemini bridge: task and file inventory alone are %d bytes, over the %d byte cap", len(prompt), limit)
		}
		if excess <= 0 {
			return prompt, ctx, dropped, nil
		}
		inc := append([]IncludedFile(nil), ctx.Included...)
		skipped := append([]SkippedFile(nil), ctx.Skipped...)
		for freed := 0; freed < excess && len(inc) > 0; {
			f := inc[len(inc)-1]
			inc = inc[:len(inc)-1]
			skipped = append(skipped, SkippedFile{f.Path, "prompt-cap"})
			dropped = append(dropped, f.Path)
			freed += len(promptBlock(f)) + 2 + len(inventoryLine(f)) // its block, separator and inventory line
		}
		ctx.Included, ctx.Skipped = inc, skipped
	}
}

func inventoryLine(f IncludedFile) string {
	return fmt.Sprintf("- %s | %s | %d bytes | truncated=%t\n", f.Path, f.MediaType, f.Bytes, f.Truncated)
}

func promptBlock(f IncludedFile) string {
	return fmt.Sprintf("<file path=%q media_type=%q truncated=%q>%s</file>",
		f.Path, f.MediaType, strconv.FormatBool(f.Truncated), f.Content)
}

// BuildPrompt renders the context inventory + inline files + task into the bridge's prompt template.
func BuildPrompt(task string, ctx Context) string {
	var inv strings.Builder
	if len(ctx.Included) > 0 {
		inv.WriteString("Included files:\n")
		for _, f := range ctx.Included {
			inv.WriteString(inventoryLine(f))
		}
	} else {
		inv.WriteString("Included files: none\n")
	}
	if len(ctx.Skipped) > 0 {
		inv.WriteString("Skipped files:\n")
		for _, s := range ctx.Skipped {
			fmt.Fprintf(&inv, "- %s (%s)\n", s.Path, s.Reason)
		}
	}

	var blocks strings.Builder
	if len(ctx.Included) == 0 {
		blocks.WriteString("No inline file payloads were collected.")
	} else {
		for i, f := range ctx.Included {
			if i > 0 {
				blocks.WriteString("\n\n")
			}
			blocks.WriteString(promptBlock(f))
		}
	}

	// Task and constraints lead: agy caps its input and cuts the tail, which must never be the question.
	return fmt.Sprintf(`<task>
%s
</task>

<constraints>
- Use the provided workspace context when it is relevant.
- Cite file paths when referring to evidence from inline context.
- Call out when the context is partial, skipped, or truncated (name the last file you saw in full).
- Do not invent files or data that are not present in the provided payloads.
</constraints>

<context_inventory>
%s</context_inventory>

<context_files>
%s
</context_files>`, task, inv.String(), blocks.String())
}

// Structured path index: a nested JSON tree names every matched path at a fraction of inlining's tokens.

// indexLeaf is what the model needs to decide whether a file is worth pulling.
type indexLeaf struct {
	Bytes int    `json:"bytes,omitempty"`
	Body  string `json:"body,omitempty"` // "inlined" | the skip reason — why the content is or isn't present
}

// BuildIndex renders ctx as a nested directory tree in JSON, including skipped files (with their reason).
func BuildIndex(ctx Context) string {
	root := map[string]any{}
	for _, f := range ctx.Included {
		insertPath(root, f.Path, indexLeaf{Bytes: f.Bytes, Body: "inlined"})
	}
	for _, f := range ctx.Skipped {
		insertPath(root, f.Path, indexLeaf{Body: f.Reason})
	}
	data, err := json.MarshalIndent(root, "", " ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

// insertPath nests leaf under its path segments; a segment colliding with an existing leaf is left alone.
func insertPath(root map[string]any, path string, leaf indexLeaf) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	node := root
	for i, p := range parts {
		if i == len(parts)-1 {
			if _, taken := node[p]; !taken {
				node[p] = leaf
			}
			return
		}
		child, ok := node[p].(map[string]any)
		if !ok {
			if _, taken := node[p]; taken {
				return // a file and a directory share a name — keep the first, skip the second
			}
			child = map[string]any{}
			node[p] = child
		}
		node = child
	}
}

// IndexPrompt renders the index as a prompt section, framing it as a routing map, not data to summarize.
func IndexPrompt(ctx Context) string {
	var b strings.Builder
	b.WriteString("\n## Repository index (paths only)\n\n")
	b.WriteString("A nested map of every matched path. `body` says whether that file's contents are in this\n")
	b.WriteString("prompt (\"inlined\") or why they are not. Use it to name exact paths; if a file you need is\n")
	b.WriteString("listed but not inlined, say which path you need rather than guessing its contents.\n\n```json\n")
	b.WriteString(BuildIndex(ctx))
	b.WriteString("\n```\n")
	return b.String()
}

// IndexPathCount reports how many files the index covers, for the operator-facing summary line.
func IndexPathCount(ctx Context) int { return len(ctx.Included) + len(ctx.Skipped) }

// CoverageWarning is a one-line STDOUT summary for when anything was left out of a sweep (the
// detailed per-reason breakdown stays on STDERR via SortedSkipReasons); "" when nothing was skipped.
func CoverageWarning(ctx Context) string {
	if len(ctx.Skipped) == 0 {
		return ""
	}
	var kb int
	for _, f := range ctx.Included {
		kb += len(f.Content)
	}
	kb /= 1024
	total := len(ctx.Included) + len(ctx.Skipped)
	return fmt.Sprintf("bridge: inlined %d of %d files (%d KB); skipped %d: %s",
		len(ctx.Included), total, kb, len(ctx.Skipped), strings.Join(SortedSkipReasons(ctx), ", "))
}

// SortedSkipReasons summarizes why files were left out, so an operator can widen the right knob.
func SortedSkipReasons(ctx Context) []string {
	counts := map[string]int{}
	for _, f := range ctx.Skipped {
		counts[f.Reason]++
	}
	out := make([]string, 0, len(counts))
	for reason := range counts {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}
