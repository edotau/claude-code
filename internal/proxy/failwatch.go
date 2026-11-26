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
	fail5xxThreshold = 5
	fail5xxWindow    = 60 * time.Second
	failLogTailBytes = 64 << 10

	failKindProxyDown = "proxy_down"
	failKindProxy5xx  = "proxy_5xx"
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

// FailWatch tracks upstream 5xx bursts per provider/status and reports them once per window.
type FailWatch struct {
	mu      sync.Mutex
	now     func() time.Time
	logPath string
	report  FailureReporter
	rings   map[string]*ring5xx
}

type ring5xx struct {
	times        [fail5xxThreshold]time.Time
	pos, count   int
	lastReported time.Time
	reporting    bool
}

var defaultFailWatch = &FailWatch{}

// NewFailWatch returns a FailWatch referencing logPath.
func NewFailWatch(logPath string, report FailureReporter) *FailWatch {
	return &FailWatch{logPath: logPath, report: report}
}

// Record5xx records an upstream status on the package-wide watcher and reports a 5xx burst once per window.
func Record5xx(provider string, status int) bool {
	return defaultFailWatch.Record(provider, status)
}

// Record records an upstream status for provider and reports proxy_5xx when 5xx hits fail5xxThreshold in fail5xxWindow.
func (w *FailWatch) Record(provider string, status int) bool {
	if status < 500 || status > 599 {
		return false
	}
	if provider == "" {
		provider = "unknown"
	}
	subject := fmt.Sprintf("%s/%d", provider, status)
	report := w.report
	if report == nil {
		report = currentFailureReporter()
	}
	if report == nil {
		return false
	}
	now, fire := w.observe(subject)
	if !fire {
		return false
	}
	logPath := w.logPath
	if logPath == "" {
		logPath = logFile()
	}
	ok := report(FailureEvent{
		Kind:    failKindProxy5xx,
		Subject: subject,
		Summary: fmt.Sprintf("%d upstream %d responses in %s", fail5xxThreshold, status, fail5xxWindow),
		Log:     logPath,
		Time:    now.Unix(),
	})
	w.finishReport(subject, now, ok)
	return ok
}

func (w *FailWatch) finishReport(subject string, now time.Time, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r := w.rings[subject]; r != nil {
		r.reporting = false
		if ok && now.After(r.lastReported) {
			r.lastReported = now
		}
	}
}

func (w *FailWatch) observe(subject string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	nowFn := w.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	if w.rings == nil {
		w.rings = make(map[string]*ring5xx)
	}
	r := w.rings[subject]
	if r == nil {
		r = &ring5xx{}
		w.rings[subject] = r
	}
	r.times[r.pos] = now
	r.pos = (r.pos + 1) % fail5xxThreshold
	if r.count < fail5xxThreshold {
		r.count++
	}
	if r.count < fail5xxThreshold || r.reporting {
		return now, false
	}
	oldest := r.times[r.pos]
	if now.Sub(oldest) > fail5xxWindow {
		return now, false
	}
	if !r.lastReported.IsZero() && now.Sub(r.lastReported) < fail5xxWindow {
		return now, false
	}
	r.reporting = true
	return now, true
}

// reportDeadDaemon logs proxy_down when proxy.json names a dead pid.
func reportDeadDaemon() bool {
	return reportDeadDaemonWith(logFile(), currentFailureReporter())
}

func reportDeadDaemonWith(logPath string, report FailureReporter) bool {
	if report == nil {
		return false
	}
	st, err := ReadState()
	if err != nil || st.PID <= 0 || alive(st.PID) {
		return false
	}
	summary := lastExitingLine(logPath, st.PID)
	if summary == "" {
		summary = fmt.Sprintf("proxy pid %d dead (no exit line in proxy.log)", st.PID)
	}
	return report(FailureEvent{
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
