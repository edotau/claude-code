package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/textutil"
)

// DefaultRecoveryCap is the event-log tail RecoveryPrompt keeps when capBytes is not positive.
const DefaultRecoveryCap = 64 << 10

// RecoveryPrompt is rec's original prompt plus a block holding the newest capBytes of its redacted event log.
func RecoveryPrompt(rec Record, capBytes int) (string, error) {
	if capBytes <= 0 {
		capBytes = DefaultRecoveryCap
	}
	log, err := recoveryLog(rec.EventsPath())
	if err != nil {
		return "", err
	}
	log = tailLines(log, capBytes)
	frame := assets.RecoveryFrame{Prompt: rec.Request.Prompt, ID: rec.ID, Status: string(rec.Status),
		Cutoff: string(rec.Cutoff), Log: log}
	if rec.Wrote != nil && len(*rec.Wrote) > 0 {
		frame.HasFiles, frame.Files = true, strings.Join(*rec.Wrote, ", ")
	}
	return assets.AgentRecovery(frame), nil
}

// recoveryLog renders the text and tool-use events of the event file at path; empty is an error.
func recoveryLog(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		renderEvent(&b, sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("recover: no text or tool events in " + path)
	}
	return b.String(), nil
}

// renderEvent appends one event line's text or tool name to b; other events and bad lines add nothing.
func renderEvent(b *strings.Builder, line []byte) {
	var ev wire.Event
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch {
	case ev.Type == wire.EventText && ev.Text != "":
		if ev.Sep {
			b.WriteByte('\n')
		}
		b.WriteString(ev.Text)
	case ev.Type == wire.EventToolUse && ev.Tool != nil:
		textutil.WriteStrings(b, "\n[tool: ", ev.Tool.Name, "]\n")
	}
}

// tailLines keeps the last n bytes of s, starting on a line boundary.
func tailLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := bytes.IndexByte([]byte(s), '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
