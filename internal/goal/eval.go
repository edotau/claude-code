package goal

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Verdict is one evaluation: Pass closes the goal; Pending (a run still going) neither passes nor blocks.
type Verdict struct {
	Pass, Pending bool
	Out           string
}

// Evaluate runs the goal's done check under ctx: its command, else its workflow run's manifest.
func Evaluate(ctx context.Context, g Goal) Verdict {
	if g.Check != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", g.Check)
		cmd.Dir = g.CWD
		cmd.WaitDelay = 300 * time.Millisecond
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			return Verdict{Out: "check timed out: " + Tail(string(out))}
		}
		return Verdict{Pass: err == nil, Out: string(out) + errText(err)}
	}
	if g.Run == "" {
		return Verdict{Out: "goal has neither a check nor a run"}
	}
	data, err := os.ReadFile(filepath.Join(g.SessionDir, "workflows", g.Run+".json"))
	if err != nil {
		return Verdict{Pending: true, Out: "run " + g.Run + " has no manifest yet"}
	}
	var m struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return Verdict{Pending: true, Out: "run " + g.Run + " manifest not readable yet: " + err.Error()}
	}
	switch m.Status {
	case "running":
		return Verdict{Pending: true, Out: "run " + g.Run + " is still running"}
	case "completed":
		return Verdict{Pass: true, Out: "run " + g.Run + " completed"}
	case "":
		return Verdict{Out: "run " + g.Run + " ended without a status"}
	}
	return Verdict{Out: "run " + g.Run + " ended " + m.Status}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return "\n" + err.Error()
}
