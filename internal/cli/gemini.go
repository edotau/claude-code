package cli

import (
	"cmp"
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

	"github.com/edotau/claude-code/internal/gemini"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdGemini is the gemini agent verb: bridge reaches the harness's own 1M-context sweep; everything
// else (launch, ask, resume, status, result, cancel, setup, models, help) is AgentVerb's job.
func cmdGemini(args []string) int {
	if len(args) > 0 && args[0] == "bridge" {
		return cmdGeminiBridge(args[1:])
	}
	return AgentVerb("gemini", args)
}

// geminiBridgePage is `gemini bridge -h`: the harness's own large-context sweep, not agy's launcher.
var geminiBridgePage = helptext.Page{
	Usage: "claude-code gemini bridge [flags] <task>",
	About: "Inline the files under --dirs/--files into one 1M-context Gemini call and stream the " +
		"answer. --diff narrows to files changed vs HEAD; --index prepends a path index.",
}

type geminiBridgeOpts struct {
	gemini.GeminiOptions
	dirs, files, format string
}

func geminiBridgeFlags() (*flag.FlagSet, *geminiBridgeOpts) {
	fs := helptext.NewFlagSet("gemini bridge", geminiBridgePage)
	o := &geminiBridgeOpts{}
	fs.StringVar(&o.Task, "task", "", "explicit task text (else trailing args)")
	fs.StringVar(&o.Model, "model", "", "alias (opus), version (opus-5-5) or wire id")
	fs.StringVar(&o.Provider, "profile", "", "API-key profile instead of agy's Google sign-in")
	fs.StringVar(&o.Effort, "effort", "", "reasoning effort: low|medium|high|xhigh")
	fs.StringVar(&o.dirs, "dirs", "", "comma-separated dirs to ingest recursively")
	fs.StringVar(&o.files, "files", "", "comma-separated file globs to ingest")
	fs.StringVar(&o.format, "format", "text", "output format: text|json")
	fs.IntVar(&o.MaxFiles, "max-files", gemini.DefaultMaxFiles, "max files to inline")
	fs.IntVar(&o.MaxFileBytes, "max-file-bytes", gemini.DefaultMaxFileBytes, "max bytes per file")
	fs.DurationVar(&o.Timeout, "timeout", 10*time.Minute, "per-attempt deadline")
	fs.BoolVar(&o.PrintCommand, "print-command", false, "print the resolved request and exit")
	fs.StringVar(&o.Rubric, "rubric", "", "review standard file quoted verbatim ahead of the task")
	fs.BoolVar(&o.Index, "index", false, "prepend a nested path index of every matched path")
	fs.BoolVar(&o.Diff, "diff", false, "ingest only files changed vs HEAD")
	return fs, o
}

func cmdGeminiBridge(args []string) int {
	fs, o := geminiBridgeFlags()
	// Interspersed: `bridge "task" --dirs d` must ingest d, not append "--dirs d" to the task.
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if code := checkGeminiBridge(o, pos); code != 0 {
		return code
	}
	o.Task = cmp.Or(o.Task, strings.Join(pos, " "))
	if strings.TrimSpace(o.Task) == "" {
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
	o.Cwd, o.Dirs, o.Files = cwd, textutil.SplitCSV(o.dirs), textutil.SplitCSV(o.files)
	o.IndexOnly = o.MaxFiles == 0
	return runGeminiBridge(o.GeminiOptions, o.format)
}

func checkGeminiBridge(o *geminiBridgeOpts, pos []string) int {
	if o.format != "text" && o.format != "json" {
		return fail("gemini bridge: --format %q unknown (text|json)", o.format)
	}
	if o.MaxFiles < 0 || o.MaxFileBytes < 0 {
		fmt.Fprintln(os.Stderr, "claude-code: gemini bridge: --max-files and --max-file-bytes must not be negative")
		return 2
	}
	if o.Task != "" && len(pos) > 0 {
		fmt.Fprintln(os.Stderr, "claude-code: gemini bridge: --task and trailing positional args are mutually exclusive")
		return 2
	}
	return 0
}

func runGeminiBridge(opts gemini.GeminiOptions, format string) int {
	var stream io.Writer = os.Stdout
	if format == "json" {
		stream = io.Discard
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := gemini.RunGemini(ctx, opts, stream, os.Stderr)
	if format == "json" {
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
