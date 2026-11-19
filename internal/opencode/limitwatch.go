package opencode

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent/retry"
)

// limitErrRE reads the error.error field of opencode's own logger line.
var limitErrRE = regexp.MustCompile(`error\.error="((?:[^"\\]|\\.)*)"`)

// maxLogLine bounds the partial line kept between writes.
const maxLogLine = 64 << 10

// limitWatch scans opencode's stderr log lines: a quota or auth "stream error" is the run's cause and ends the child.
type limitWatch struct {
	kill func() error
	mu   sync.Mutex
	buf  []byte
	err  error
}

// watchLimit tees cmd's stderr into a limitWatch whose Err feeds StreamOpts.Cause.
func watchLimit(cmd *exec.Cmd) func() error {
	w := &limitWatch{kill: func() error { return cmd.Cancel() }}
	if cmd.Stderr != nil {
		cmd.Stderr = io.MultiWriter(cmd.Stderr, w)
	} else {
		cmd.Stderr = w
	}
	return w.Err
}

func (w *limitWatch) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *limitWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	var hit *retry.Error
	for hit == nil {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		hit = limitCause(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLogLine {
		w.buf = nil
	}
	first := hit != nil && w.err == nil
	if first {
		w.err = hit
	}
	w.mu.Unlock()
	if first {
		_ = w.kill()
	}
	return len(p), nil
}

// limitCause is the quota or auth failure a logger line reports, else nil; transient kinds stay with opencode's retry.
func limitCause(line string) *retry.Error {
	if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, `message="stream error"`) {
		return nil
	}
	// only the root run's own model stream decides: opencode tolerates a failed title or small-model call
	if !strings.Contains(line, " small=false ") || !strings.Contains(line, " mode=primary ") {
		return nil
	}
	m := limitErrRE.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	reason, err := strconv.Unquote(`"` + m[1] + `"`)
	if err != nil {
		reason = m[1]
	}
	// the console link holds an org id: keep the sentence that states the limit and its reset
	reason, _, _ = strings.Cut(reason, ". ")
	reason, _, _ = strings.Cut(reason, "http")
	e := retry.Classify("opencode", fmt.Errorf("opencode: %w", errors.New(strings.TrimSpace(reason))), false)
	if e.Kind != retry.KindQuota && e.Kind != retry.KindAuth {
		return nil
	}
	return e
}
