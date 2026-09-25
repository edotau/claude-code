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
)

const geminiVerbUsage = `claude-code gemini — the gemini agent

USAGE
  claude-code gemini [vendor args…]      launch the vendor gemini CLI, args untouched
  claude-code gemini ask …               one-shot on the gemini CLI runner (= ask --agent gemini --provider gemini)
  claude-code gemini bridge [flags] …    in-process large-context bridge (--dirs/--files/--task)
`

// cmdGemini is the gemini agent verb: ask/bridge reach the harness's own runners; everything else
// launches the vendor gemini CLI with the args untouched.
func cmdGemini(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "ask":
			return cmdAsk(append([]string{"--agent", "gemini", "--provider", "gemini"}, args[1:]...))
		case "bridge":
			return cmdGeminiBridge(args[1:])
		}
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprint(os.Stderr, geminiVerbUsage)
		return 2
	}
	return launchVendor("gemini", args)
}

// splitCSV splits a comma-separated flag value, trimming blanks and dropping empty entries.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func cmdGeminiBridge(args []string) int {
	fs := flag.NewFlagSet("gemini bridge", flag.ContinueOnError)
	task := fs.String("task", "", "explicit task text (else trailing args)")
	model := fs.String("model", "", "slot name, model id, or provider:model")
	provider := fs.String("provider", "gemini", "provider to run against")
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "text" && *format != "json" {
		return fail("gemini bridge: --format %q unknown (text|json)", *format)
	}
	if *maxFiles < 0 || *maxFileBytes < 0 {
		fmt.Fprintln(os.Stderr, "usage: gemini bridge: --max-files and --max-file-bytes must not be negative")
		return 2
	}
	if *task != "" && fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: gemini bridge: --task and trailing positional args are mutually exclusive")
		return 2
	}
	taskText := *task
	if taskText == "" {
		taskText = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(taskText) == "" {
		fmt.Fprintln(os.Stderr, "usage: claude-code gemini bridge [flags] <task>")
		fs.PrintDefaults()
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
