package ask

import (
	"context"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Explore is `<agent> explore`: scan the code (and the web when web is set) for question and return a brief.
func Explore(name, question string, web, background bool, format string) int {
	cwd, _ := os.Getwd()
	askArgs, err := roles.Args(name, cwd, question, web, background)
	if err != nil {
		return helptext.Fail("%s explore: %v", name, err)
	}
	if format != helptext.Text {
		task := askArgs[len(askArgs)-1]
		askArgs = append(askArgs[:len(askArgs)-1], "--format", format, task)
	}
	return Cmd(askArgs)
}

// Transfer is `<agent> transfer`: hand the source transcript (default: this repo's newest) to name with note.
func Transfer(name, source, note string, wait bool) int {
	cwd, _ := os.Getwd()
	src, err := transcript.ResolveSource(source, cwd)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	fmt.Fprintf(os.Stderr, "transfer: source %s\n", src)
	prompt, err := transcript.Handoff(src, cwd, note)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	code := Cmd(transcript.TransferArgs(name, cwd, prompt, wait))
	if code == 0 && wait {
		if rec, _, err := job.JobResult(context.Background(), name, cwd, ""); err == nil {
			PrintResumeHint(name, rec.SessionID)
		}
	}
	return code
}

// PrintResumeHint prints the agent's resume command for a session; nothing when it has none.
func PrintResumeHint(name, sessionID string) {
	s, _ := agent.LookupSchema(name)
	if hint := s.ResumeHint(sessionID); hint != "" {
		fmt.Printf("resume: %s\n", hint)
	}
}
