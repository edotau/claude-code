package ask

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/live"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// liveExclusive are the ask flags a live session cannot honor: it has no single task, route or envelope of its own.
var liveExclusive = []string{"background", "host", "batch", "retry", "job", "env", "attach", "no-tools", "output-schema",
	"rubric", "log", "stdin-framed", "fallback", "native"}

// askLiveCheck rejects --live with another agent, a task, or a flag that has no meaning for a held-open session.
func askLiveCheck(fs *flag.FlagSet, name string) error {
	if row, _ := agent.LookupSchema(name); !row.HasLive() {
		return fmt.Errorf("--live is claude or gemini only, not %s", name)
	}
	if fs.NArg() > 0 {
		return errors.New("--live takes no task: start it, then claude-code agents send <id> <task>")
	}
	for _, f := range liveExclusive {
		if helptext.Explicit(fs, f) {
			return fmt.Errorf("--live cannot be combined with --%s", f)
		}
	}
	return nil
}

func startLive(s *askState, name, format string) int {
	r, err := agent.Lookup(name)
	if err == nil {
		err = r.Available()
	}
	if err == nil {
		err = live.CheckLiveCap("")
	}
	if err != nil {
		return failAsk(format, name, err)
	}
	req := agent.RunOptions{Model: *s.Model, Provider: *s.Provider, Effort: *s.Effort, WorkDir: *s.Workdir, SessionID: *s.Session,
		Tools: *s.Tools, Timeout: *s.Timeout, KeepEnv: *s.KeepEnv, NoPersist: *s.NoPersist, SafeMode: *s.SafeMode,
		DebugFile: absDebugFile(*s.DebugFile), SystemPrompt: *s.SystemPrompt, AppendSystemPrompt: *s.AppendSystemPrompt,
		Autonomous: *s.Autonomous, Direct: *s.Direct, Agent: name, Background: true}
	rec, err := NewClient().Record(context.Background(), req, nil)
	if err != nil {
		return failAsk(format, name, err)
	}
	rec.Live = true
	if err := rec.Save(); err != nil {
		return failAsk(format, name, err)
	}
	if err := job.Spawn(&rec, nil); err != nil {
		abandonLive(&rec, err)
		return failAsk(format, name, err)
	}
	if err := live.WaitLiveReady(rec.ID, rec.PID); err != nil {
		abandonLive(&rec, err)
		return failAsk(format, name, fmt.Errorf("live session %s did not start: %w (log: %s)", rec.ID, err, rec.Log))
	}
	if format == helptext.JSON {
		body, _ := json.Marshal(struct {
			ID     string `json:"id"`
			Socket string `json:"socket"`
			Log    string `json:"log"`
		}{rec.ID, live.LiveSocket(rec.ID), rec.Log})
		fmt.Println(string(body))
		return 0
	}
	fmt.Printf("live session %s — claude-code agents send %s \"<task>\" · agents cancel %s · idle limit %s\n", rec.ID, rec.ID, rec.ID, live.IdleOf(req))
	return 0
}

// abandonLive stops a holder that never became ready and marks its saved record failed.
func abandonLive(rec *agent.Record, cause error) {
	if rec.PID > 0 {
		_ = job.Cancel(rec)
	}
	if rec.Status == "running" || rec.Status == "" {
		rec.Status, rec.Error = "error", cause.Error()
		_ = rec.Save()
	}
}

// runLiveHolder is live.RunHolder over the cli client's launcher.
func runLiveHolder(rec *agent.Record) int { return live.RunHolder(rec, NewClient().Launcher) }
