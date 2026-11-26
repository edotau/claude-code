package proxy

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	failLogTailBytes  = 64 << 10
	failKindProxyDown = "proxy_down"
)

// FailureEvent is a proxy failure payload forwarded to the registered worktree.ReportFailure bridge.
type FailureEvent struct {
	Kind    string
	Subject string
	Summary string
	Log     string
	Time    int64
}

// FailureReporter delivers a FailureEvent; returns true when recorded.
type FailureReporter func(FailureEvent) bool

var (
	reporterMu      sync.RWMutex
	failureReporter FailureReporter
)

// RegisterFailureReporter wires worktree.ReportFailure into proxy without a proxy->worktree import cycle.
func RegisterFailureReporter(fn FailureReporter) {
	reporterMu.Lock()
	failureReporter = fn
	reporterMu.Unlock()
}

func currentFailureReporter() FailureReporter {
	reporterMu.RLock()
	fn := failureReporter
	reporterMu.RUnlock()
	return fn
}

// reportDeadDaemon logs proxy_down when proxy.json names a dead pid.
func reportDeadDaemon() {
	report := currentFailureReporter()
	if report == nil {
		return
	}
	st, err := ReadState()
	if err != nil || st.PID <= 0 || alive(st.PID) {
		return
	}
	logPath := logFile()
	summary := lastExitingLine(logPath, st.PID)
	if summary == "" {
		summary = fmt.Sprintf("proxy pid %d dead (no exit line in proxy.log)", st.PID)
	}
	report(FailureEvent{
		Kind:    failKindProxyDown,
		Subject: fmt.Sprintf("pid/%d", st.PID),
		Summary: summary,
		Log:     logPath,
		Time:    time.Now().Unix(),
	})
}

// lastExitingLine returns the newest "proxy: exiting (pid <pid>)" line from the tail of path, or "".
func lastExitingLine(path string, pid int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > failLogTailBytes {
		_, _ = f.Seek(-failLogTailBytes, io.SeekEnd)
	}
	raw, err := io.ReadAll(io.LimitReader(f, failLogTailBytes))
	if err != nil {
		return ""
	}
	needle := fmt.Sprintf("proxy: exiting (pid %d)", pid)
	lines := bytes.Split(raw, []byte("\n"))
	for _, l := range slices.Backward(lines) {
		line := strings.TrimSpace(string(l))
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
