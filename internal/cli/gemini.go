package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
)

// cmdGemini is the gemini agent verb: bridge reaches the harness's own 1M-context sweep; everything
// else (launch, ask, resume, status, result, cancel, setup, models, help) is vendorVerb's job.
func cmdGemini(args []string) int {
	if len(args) > 0 && args[0] == "bridge" {
		return cmdGeminiBridge(args[1:])
	}
	return vendorVerb("gemini", args)
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

// geminiBridgePage is `gemini bridge -h`: the harness's own large-context sweep, not agy's launcher.
var geminiBridgePage = helptext.Page{
	Usage: "claude-code gemini bridge [flags] <task>",
	About: "Inline the files under --dirs/--files into one 1M-context Gemini call and stream the " +
		"answer. --diff narrows to files changed vs HEAD; --index prepends a path index.",
}

func cmdGeminiBridge(args []string) int {
	fs := helptext.NewFlagSet("gemini bridge", geminiBridgePage)
	task := fs.String("task", "", "explicit task text (else trailing args)")
	model := fs.String("model", "", "alias (opus), version (opus-5-5) or wire id")
	provider := fs.String("profile", "", "API-key profile instead of agy's Google sign-in")
	effort := fs.String("effort", "", "reasoning effort: low|medium|high|xhigh")
	dirs := fs.String("dirs", "", "comma-separated dirs to ingest recursively")
	files := fs.String("files", "", "comma-separated file globs to ingest")
	format := fs.String("format", "text", "output format: text|json")
	maxFiles := fs.Int("max-files", agent.DefaultMaxFiles, "max files to inline")
	maxFileBytes := fs.Int("max-file-bytes", agent.DefaultMaxFileBytes, "max bytes per file")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-attempt deadline")
	printCommand := fs.Bool("print-command", false, "print the resolved request and exit")
	rubric := fs.String("rubric", "", "review standard file quoted verbatim ahead of the task")
	index := fs.Bool("index", false, "prepend a nested path index of every matched path")
	diff := fs.Bool("diff", false, "ingest only files changed vs HEAD")
	// Interspersed: `bridge "task" --dirs d` must ingest d, not append "--dirs d" to the task.
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *format != "text" && *format != "json" {
		return fail("gemini bridge: --format %q unknown (text|json)", *format)
	}
	if *maxFiles < 0 || *maxFileBytes < 0 {
		fmt.Fprintln(os.Stderr, "claude-code: gemini bridge: --max-files and --max-file-bytes must not be negative")
		return 2
	}
	if *task != "" && len(pos) > 0 {
		fmt.Fprintln(os.Stderr, "claude-code: gemini bridge: --task and trailing positional args are mutually exclusive")
		return 2
	}
	taskText := cmp.Or(*task, strings.Join(pos, " "))
	if strings.TrimSpace(taskText) == "" {
		fs.Usage()
		if len(args) == 0 {
			return 0 // bare `gemini bridge` is a help request
		}
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
	opts := agent.GeminiOptions{
		Task: taskText, Model: *model, Provider: *provider, Effort: *effort, Rubric: *rubric, Cwd: cwd,
		Dirs: splitCSV(*dirs), Files: splitCSV(*files),
		MaxFiles: *maxFiles, MaxFileBytes: *maxFileBytes, Timeout: *timeout, IndexOnly: *maxFiles == 0,
		Index: *index, Diff: *diff, PrintCommand: *printCommand,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := agent.RunGemini(ctx, opts, stream, os.Stderr)
	if *format == "json" {
		body, merr := json.Marshal(agent.NewEnvelope(res, err, ""))
		if merr != nil {
			return fail("gemini bridge: encoding result: %v", merr)
		}
		fmt.Println(string(body))
	} else if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
	if err != nil {
		return fail("gemini bridge: %v", err)
	}
	return 0
}
