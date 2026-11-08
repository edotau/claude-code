package ask

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// askState is the agent.AskTarget plus the flag pointers --retry resets or --job clears.
type askState struct {
	client.AskTarget
	rubric *string
	env    *envFlag
}

// loadAskJob loads a --job record for a background run continuation; rc/done are set only on an early return.
func loadAskJob(job, format string, s *askState, prompt *string) (rec *job.Record, rc int, done bool) {
	if job == "" {
		return nil, 0, false
	}
	r, err := client.LoadJobRecord(job)
	if errors.Is(err, client.ErrJobNotFresh) {
		return nil, failAskCode(format, *s.Name, err, 2), true
	}
	if err != nil {
		return nil, failAsk(format, *s.Name, err), true
	}
	s.Apply(r, os.Stderr)
	*prompt, *s.rubric = r.Request.Prompt, ""
	return &r, 0, false
}

// loadAskRetry loads a --retry record, then re-applies any flags explicitly typed on this command line.
func loadAskRetry(fs *flag.FlagSet, args []string, retry, format string, s *askState, prompt *string) (rc int, done bool) {
	if retry == "" {
		return 0, false
	}
	rec, err := job.LoadRecord(retry)
	if err != nil {
		return failAsk(format, *s.Name, err), true
	}
	// The logged request wins; only flags typed on this command line override it.
	s.Apply(rec, os.Stderr)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "attach" {
			*s.Attach = nil
		}
		if f.Name == "keep-env" {
			*s.KeepEnv = nil
		}
		if f.Name == "allow" {
			*s.PermAllow = nil
		}
		if f.Name == "ask" {
			*s.PermAsk = nil
		}
		if f.Name == "deny" {
			*s.PermDeny = nil
		}
		if f.Name == "env" {
			*s.env = nil
		}
	})
	if err := fs.Parse(args); err != nil { // re-apply explicit flags over the record
		return failAskCode(format, *s.Name, err, 2), true
	}
	*prompt, *s.rubric = rec.Request.Prompt, ""
	fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	return 0, false
}

// printDispatchLog lists recent dispatches; the prompt preview is scrubbed of credential shapes.
func printDispatchLog(format string) int {
	recs := job.RecentRecords(20)
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("ask", recs)
	}
	if len(recs) == 0 {
		fmt.Println("no dispatches logged yet")
		return 0
	}
	for _, r := range recs {
		fmt.Print(r.LogLine())
	}
	return 0
}
