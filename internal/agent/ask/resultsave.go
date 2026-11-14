package ask

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// resultText is the text `agents result` prints: the log, the ask line when full, then the session and resume command.
func resultText(rec agent.Record, log string, full bool) string {
	var b strings.Builder
	b.WriteString(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		b.WriteByte('\n')
	}
	if full {
		if line := rec.ResultEnvelope(os.Stderr).RanOn; line != "" {
			fmt.Fprintln(&b, "ask:", line)
		}
	}
	if rec.SessionID != "" {
		fmt.Fprintf(&b, "session: %s\n", rec.SessionID)
		fprintResumeHint(&b, rec.Agent, rec.SessionID)
	}
	return b.String()
}

// SaveResult is `agents result <id> --save <path>`: it writes the plain result text to path and prints one summary
// line (json: {path, bytes, lines}); head > 0 follows it with the first head lines. Exit codes follow the result's.
func SaveResult(rec agent.Record, path string, head int, format string) int {
	cwd, err := os.Getwd()
	var log string
	if err == nil {
		rec, log, err = agent.JobResult(context.Background(), rec.Agent, cwd, rec.ID)
	}
	if err != nil {
		return helptext.Fail("agents result: %v", err)
	}
	text := resultText(rec, log, true)
	if err := paths.AtomicWrite(path, []byte(text), 0o600); err != nil {
		return helptext.Fail("agents result: save %s: %v", path, err)
	}
	lines := strings.Count(text, "\n")
	if format == helptext.JSON {
		if code := helptext.WriteJSONOrFail("agents result", struct {
			Path  string `json:"path"`
			Bytes int    `json:"bytes"`
			Lines int    `json:"lines"`
		}{path, len(text), lines}); code != 0 {
			return code
		}
	} else {
		fmt.Printf("saved %d B, %d lines -> %s\n", len(text), lines, path)
		if head > 0 {
			fmt.Print(firstLines(text, head))
		}
	}
	return resultExit(rec)
}

// firstLines is the first n lines of text, newline included.
func firstLines(text string, n int) string {
	end := 0
	for ; n > 0; n-- {
		i := strings.IndexByte(text[end:], '\n')
		if i < 0 {
			return text
		}
		end += i + 1
	}
	return text[:end]
}
