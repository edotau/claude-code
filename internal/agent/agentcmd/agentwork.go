package agentcmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// agentResume is `claude-code <v> resume <id> <task…>` = `ask --agent <v> --session <id> <task…>`.
func agentResume(name string, args []string) int {
	if agentSubHelp(name, "resume", args) {
		return 0
	}
	if len(args) < 1 {
		return helptext.Fail("%s resume: usage: claude-code %s resume <id> <task>", name, name)
	}
	if strings.HasPrefix(args[0], "-") { // flags first: ask's own parser, the session id is its first positional
		return ask.Run(append([]string{"--agent", name}, args...), true)
	}
	return ask.Cmd(append([]string{"--agent", name, "--session", args[0]}, args[1:]...))
}

// printResumeHint prints the agent's resume command for a session; nothing when it has none (ResumeHint("") is "").
func printResumeHint(s agent.Schema, sessionID string) {
	if hint := s.ResumeHint(sessionID); hint != "" {
		fmt.Printf("resume: %s\n", hint)
	}
}

// agentSetup is `claude-code <v> setup [--format text|json]`.
func agentSetup(name string, args []string) int {
	fs := newAgentFlags(name, "setup")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat(name+" setup", err)
	}
	row, ok := agent.LookupSchema(name)
	if !ok {
		row = agent.Schema{Name: name} // reported as an unknown agent, by name
	}
	rep := row.Setup()
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rep); err != nil {
			return helptext.Fail("%s setup: %v", name, err)
		}
	} else {
		for _, c := range rep.Checks {
			fmt.Printf("%s  %s  %s  %s\n", rep.Agent, c.Name, c.State, c.Detail)
		}
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// agentReview is `claude-code <v> review [scope flags] [--adversarial] [--background] [--rubric f] [focus…]`.
func agentReview(name string, args []string) int {
	fs := newAgentFlags(name, "review")
	uncommitted := fs.Bool("uncommitted", false, "working tree + index vs HEAD (default)")
	base := fs.String("base", "", "diff against this branch (triple-dot merge base)")
	commit := fs.String("commit", "", "diff this single commit")
	pr := fs.Bool("pr", false, "diff against the remote's default branch")
	adversarial := fs.Bool("adversarial", false, "adversarial findings instead of a neutral verdict")
	background := fs.Bool("background", false, "detach; claude-code "+name+" status|result to follow up")
	rubric := fs.String("rubric", "", "grade the review against this rubric file")
	model := fs.String("model", "", "model for the review (default: the agent's pin)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	scope, err := roles.ParseScope(*uncommitted, *base, *commit, *pr)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	cwd, _ := os.Getwd()
	diff, err := roles.Diff(context.Background(), cwd, scope)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	focus := strings.Join(fs.Args(), " ")
	if parts := roles.ReviewParts(diff, *adversarial, focus, *rubric); len(parts) > 1 {
		return runSplitReview(name, args, parts, scope, *adversarial, *background, focus, *rubric, *model)
	}
	askArgs, cleanup, err := roles.ReviewArgs(name, scope, diff, *adversarial, *background, focus, *rubric, *model)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	defer cleanup()
	return ask.Cmd(askArgs)
}

// agentExplore is `claude-code <v> explore [--no-web] [--background] [--format f] <question>`.
func agentExplore(name string, args []string) int {
	fs := newAgentFlags(name, "explore")
	noWeb := fs.Bool("no-web", false, "skip the web-research step; code scan only")
	background := fs.Bool("background", false, "detach; claude-code "+name+" status|result to follow up")
	format := fs.String("format", "text", "stream text or print the JSON result envelope")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	question := strings.Join(fs.Args(), " ")
	if question == "" {
		return helptext.Fail("%s explore: usage: claude-code %s explore [--no-web] [--background] <question>", name, name)
	}
	cwd, _ := os.Getwd()
	askArgs := roles.Args(name, cwd, question, !*noWeb, *background)
	if *format != "text" {
		task := askArgs[len(askArgs)-1]
		askArgs = append(askArgs[:len(askArgs)-1], "--format", *format, task)
	}
	return ask.Cmd(askArgs)
}

// agentTransfer is `claude-code <v> transfer [--source f] [--wait] [note…]`.
func agentTransfer(name string, args []string) int {
	fs := newAgentFlags(name, "transfer")
	source := fs.String("source", "", "transcript .jsonl (default: newest session for this repo)")
	wait := fs.Bool("wait", false, "block for the answer instead of detaching")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	cwd, _ := os.Getwd()
	src, err := transcript.ResolveSource(*source, cwd)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	fmt.Fprintf(os.Stderr, "transfer: source %s\n", src)
	note := strings.Join(fs.Args(), " ")
	prompt, err := transcript.Handoff(src, cwd, note)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	code := ask.Cmd(transcript.TransferArgs(name, cwd, prompt, *wait))
	if code == 0 && *wait {
		if rec, _, err := job.JobResult(context.Background(), name, cwd, ""); err == nil {
			s, _ := agent.LookupSchema(name)
			printResumeHint(s, rec.SessionID)
		}
	}
	return code
}
