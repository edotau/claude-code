package ask

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
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

// loadResult re-reads rec's job and its log text from the working directory.
func loadResult(rec agent.Record) (agent.Record, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return rec, "", err
	}
	return agent.JobResult(context.Background(), rec.Agent, cwd, rec.ID)
}

// SaveResult is `agents result <id> --save <path>`: it writes the plain result text to path and prints one summary
// line (json: {path, bytes, lines}); head > 0 follows it with the first head lines. Exit codes follow the result's.
func SaveResult(rec agent.Record, path string, head int, format string) int {
	rec, log, err := loadResult(rec)
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

// Verdict is `agents result <id> --verdict`: one status line then the last n non-empty answer lines (json: the
// envelope plus a verdict field). Exit codes follow the result's.
func Verdict(rec agent.Record, n int, format string) int {
	rec, log, err := loadResult(rec)
	if err != nil {
		return helptext.Fail("agents result: %v", err)
	}
	tail := lastLines(log, n)
	if format == helptext.JSON {
		env := struct {
			wire.Envelope
			Verdict []string `json:"verdict"`
		}{rec.ResultEnvelope(os.Stderr), tail}
		if code := helptext.WriteJSONOrFail("agents result", env); code != 0 {
			return code
		}
	} else {
		fmt.Printf("%s %s %s %s\n", rec.ID, rec.Agent, rec.Status, (time.Duration(rec.DurationMs) * time.Millisecond).String())
		for _, l := range tail {
			fmt.Println(l)
		}
	}
	return resultExit(rec)
}

// footerPrefixes start the trailing ask/session/resume lines a log ends with; they are not part of the answer.
var footerPrefixes = []string{"ask: ", "session: ", "resume: "}

// lastLines is the last n non-empty answer lines of text (footers and `▸ ` trace lines dropped), never nil.
func lastLines(text string, n int) []string {
	lines, raw, assistant := []string{}, []string{}, []string{}
	for l := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.TrimSpace(l) == "":
			continue
		case strings.HasPrefix(l, "▸ assistant: "):
			assistant = append(assistant, l)
		case !strings.HasPrefix(l, "▸ "):
			raw = append(raw, l)
		}
		lines = append(lines, l)
	}
	if len(raw) > 0 { // the answer is the plain lines, else the assistant trace lines (opencode, claude), else all
		lines = raw
	} else if len(assistant) > 0 {
		lines = assistant
	}
	for len(lines) > 0 && isFooter(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func isFooter(line string) bool {
	for _, p := range footerPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}
