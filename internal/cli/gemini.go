package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
)

const geminiVerbUsage = `claude-code gemini — the gemini agent

USAGE
  claude-code gemini [vendor args…]      launch agy; forward args unchanged
  claude-code gemini ask …              one-shot on agy (= ask --agent gemini)
  claude-code gemini bridge [flags] …    ingest files for a large-context task
`

// cmdGemini is the gemini agent verb: ask/bridge reach the harness's own runners; everything else
// launches agy (the Antigravity CLI) with the args untouched.
func cmdGemini(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "ask":
			return cmdAsk(append([]string{"--agent", "gemini"}, args[1:]...))
		case "bridge":
			return cmdGeminiBridge(args[1:])
		}
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		helptext.Fprint(os.Stderr, geminiVerbUsage)
		return 2
	}
	return launchVendor("gemini", args)
}

// splitCSV splits a comma-separated flag value, trimming blanks and dropping empty entries.
func splitCSV(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func cmdGeminiBridge(args []string) int {
	fs := flag.NewFlagSet("gemini bridge", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	task := fs.String("task", "", "explicit task text (else trailing args)")
	model := fs.String("model", "", "slot name, model id, or provider:model")
	provider := fs.String("provider", "", "API-key provider instead of agy's Google sign-in")
	effort := fs.String("effort", "", "reasoning effort: low|medium|high|xhigh")
	dirs := fs.String("dirs", "", "comma-separated dirs to ingest recursively")
	files := fs.String("files", "", "comma-separated file globs to ingest")
	format := fs.String("format", "text", "output format: text|json")
	maxFiles := fs.Int("max-files", gemini.DefaultMaxFiles, "max files to inline")
	maxFileBytes := fs.Int("max-file-bytes", gemini.DefaultMaxFileBytes, "max bytes per file")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-attempt deadline")
	printCommand := fs.Bool("print-command", false, "print the resolved request and exit")
	rubric := fs.String("rubric", "", "review standard file quoted verbatim ahead of the task")
	index := fs.Bool("index", false, "prepend a nested path index of every matched path")
	diff := fs.Bool("diff", false, "ingest only files changed vs HEAD")
	// Interspersed: `bridge "task" --dirs d` must ingest d, not append "--dirs d" to the task.
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return 2
	}
	if *format != "text" && *format != "json" {
		return fail("gemini bridge: --format %q unknown (text|json)", *format)
	}
	if *maxFiles < 0 || *maxFileBytes < 0 {
		fmt.Fprintln(os.Stderr, "usage: gemini bridge: --max-files and --max-file-bytes must not be negative")
		return 2
	}
	if *task != "" && len(pos) > 0 {
		fmt.Fprintln(os.Stderr, "usage: gemini bridge: --task and trailing positional args are mutually exclusive")
		return 2
	}
	taskText := *task
	if taskText == "" {
		taskText = strings.Join(pos, " ")
	}
	if strings.TrimSpace(taskText) == "" {
		helptext.Fprint(os.Stderr, "usage: claude-code gemini bridge [flags] <task>\n")
		helptext.Defaults(fs)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail("gemini bridge: %v", err)
	}
	var stream io.Writer = os.Stdout
	if *format == "json" {
		stream = io.Discard
	}
	opts := gemini.Options{
		Task: taskText, Model: *model, Provider: *provider, Effort: *effort, Rubric: *rubric, Cwd: cwd,
		Dirs: splitCSV(*dirs), Files: splitCSV(*files),
		MaxFiles: *maxFiles, MaxFileBytes: *maxFileBytes, Timeout: *timeout, IndexOnly: *maxFiles == 0,
		Index: *index, Diff: *diff, PrintCommand: *printCommand,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := gemini.Run(ctx, opts, stream, os.Stderr)
	if *format == "json" {
		env := struct {
			agent.Result
			Error string `json:"error,omitempty"`
		}{Result: res}
		if err != nil {
			env.Error = err.Error()
		}
		body, _ := json.Marshal(env)
		fmt.Println(string(body))
	} else if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
	if err != nil {
		return fail("gemini bridge: %v", err)
	}
	return 0
}
