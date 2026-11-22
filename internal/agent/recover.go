package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent/wire"
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
	var b strings.Builder
	b.WriteString(rec.Request.Prompt)
	fmt.Fprintf(&b, "\n\nAn earlier attempt (dispatch %s, status %s", rec.ID, rec.Status)
	if rec.Cutoff != "" {
		fmt.Fprintf(&b, ", cutoff %s", rec.Cutoff)
	}
	b.WriteString(") stopped before it finished. The log below is what it did.")
	if rec.Wrote != nil && len(*rec.Wrote) > 0 {
		fmt.Fprintf(&b, " Files it changed: %s.", strings.Join(*rec.Wrote, ", "))
	}
	b.WriteString(" Check the working tree before redoing any step, then continue from where it stopped" +
		" and finish with the report the brief asked for.\n\n--- earlier attempt log ---\n")
	b.WriteString(log)
	return b.String(), nil
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
		fmt.Fprintf(b, "\n[tool: %s]\n", ev.Tool.Name)
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
