package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
)

// ModelUsage totals one model's token counts.
type ModelUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
}

// Total sums every counted token kind.
func (m *ModelUsage) Total() int64 {
	return m.InputTokens + m.OutputTokens + m.CacheReadTokens + m.CacheCreationTokens
}

// Add adds o's four token counts to m.
func (m *ModelUsage) Add(o ModelUsage) {
	m.InputTokens += o.InputTokens
	m.OutputTokens += o.OutputTokens
	m.CacheReadTokens += o.CacheReadTokens
	m.CacheCreationTokens += o.CacheCreationTokens
}

// FileSummary is one transcript: per-model totals, API responses (Turns) and the timestamp span.
type FileSummary struct {
	Models     map[string]*ModelUsage
	Turns      int
	ModelTurns map[string]int // model id -> turn count, same dedupe-by-message.id as Models
	First      time.Time      // zero when no counted record had an RFC3339 timestamp
	Last       time.Time
	perMin     map[int64]int64 // unix minute → input tokens, feeds the aggregate's peak
}

// Totals sums every model in the file.
func (s *FileSummary) Totals() ModelUsage {
	var t ModelUsage
	for _, m := range s.Models {
		t.Add(*m)
	}
	return t
}

type usageLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			CacheCreationInput int64 `json:"cache_creation_input_tokens"`
			CacheReadInput     int64 `json:"cache_read_input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// Response is one API response, counted once; At is zero when the record carried no RFC3339 timestamp.
type Response struct {
	Model string
	Usage ModelUsage
	At    time.Time
}

// ReadResponses lists each API response once: Claude Code writes one line per content block, each repeating
// the response's usage under one message.id, so the last line per id wins (output grows while streaming).
func ReadResponses(path string) ([]Response, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byID := map[string]int{}
	var resps []Response
	scanErr := EachLine(f, func(line []byte) {
		var rec usageLine
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.Usage == nil {
			return
		}
		u := rec.Message.Usage
		r := Response{Model: rec.Message.Model, Usage: ModelUsage{u.InputTokens, u.OutputTokens, u.CacheReadInput, u.CacheCreationInput}}
		r.At, _ = time.Parse(time.RFC3339, rec.Timestamp)
		if i, seen := byID[rec.Message.ID]; seen && rec.Message.ID != "" {
			resps[i] = r
			return
		}
		byID[rec.Message.ID] = len(resps)
		resps = append(resps, r)
	})
	if scanErr != nil {
		return nil, fmt.Errorf("read %s: %w", path, scanErr)
	}
	return resps, nil
}

// SummarizeFile totals every response in one transcript file (see ReadResponses).
func SummarizeFile(path string) (*FileSummary, error) {
	resps, err := ReadResponses(path)
	if err != nil {
		return nil, err
	}
	return SummarizeResponses(resps), nil
}

// SummarizeResponses totals a set of responses, e.g. the in-window subset of one file.
func SummarizeResponses(resps []Response) *FileSummary {
	s := &FileSummary{Models: map[string]*ModelUsage{}, Turns: len(resps), ModelTurns: map[string]int{}, perMin: map[int64]int64{}}
	for _, r := range resps {
		s.count(r)
	}
	return s
}

func (s *FileSummary) count(r Response) {
	m := s.Models[r.Model]
	if m == nil {
		m = &ModelUsage{}
		s.Models[r.Model] = m
	}
	m.Add(r.Usage)
	s.ModelTurns[r.Model]++
	if r.At.IsZero() {
		return
	}
	// ITPM counts uncached input: fresh input plus cache writes; cache reads don't count on current models.
	s.perMin[r.At.Truncate(time.Minute).Unix()] += r.Usage.InputTokens + r.Usage.CacheCreationTokens
	if s.First.IsZero() || r.At.Before(s.First) {
		s.First = r.At
	}
	if r.At.After(s.Last) {
		s.Last = r.At
	}
}

// UsageAggregate totals many transcripts. PeakInputTokensPerMin is the 429-risk proxy: the highest one-minute
// sum of uncached input (input + cache writes) across all concurrent subagents.
type UsageAggregate struct {
	Models                map[string]*ModelUsage `json:"models"`
	PeakInputTokensPerMin int64                  `json:"peak_input_tokens_per_min"`

	perMin map[int64]int64
}

// NewUsageAggregate returns an empty aggregate.
func NewUsageAggregate() *UsageAggregate {
	return &UsageAggregate{Models: map[string]*ModelUsage{}, perMin: map[int64]int64{}}
}

// Add folds one file summary in.
func (a *UsageAggregate) Add(s *FileSummary) {
	for model, u := range s.Models {
		m := a.Models[model]
		if m == nil {
			m = &ModelUsage{}
			a.Models[model] = m
		}
		m.Add(*u)
	}
	for minute, n := range s.perMin {
		a.perMin[minute] += n
		a.PeakInputTokensPerMin = max(a.PeakInputTokensPerMin, a.perMin[minute])
	}
}

// WorkflowRunDirs lists the subagents/workflows/<runId> dirs of a session dir; the path may hold glob wildcards.
func WorkflowRunDirs(sessionDir string) []string {
	matches, err := filepath.Glob(filepath.Join(sessionDir, "subagents", "workflows", "*"))
	if err != nil {
		return nil
	}
	return keepDirs(matches)
}

// keepDirs filters a glob result down to directories, sorted.
func keepDirs(matches []string) []string {
	var dirs []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			dirs = append(dirs, m)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// WorkflowAgentFiles lists one run dir's agent transcripts; the agent-*.jsonl shape keeps the sibling
// *.meta.json sidecars out of the token scan.
func WorkflowAgentFiles(runDir string) []string {
	matches, err := filepath.Glob(filepath.Join(runDir, "agent-*.jsonl"))
	if err != nil {
		return nil
	}
	slices.Sort(matches)
	return matches
}

// WorkflowPhaseFromMeta reads the sibling agent-*.meta.json's workflowPhase; plenty of real metas
// carry none, so "" is a normal answer routed to `(none)` .
func WorkflowPhaseFromMeta(transcriptPath string, errw io.Writer) string {
	raw, err := os.ReadFile(strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var meta struct {
		WorkflowPhase string `json:"workflowPhase"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		fmt.Fprintf(errw, "usage: warning: skipping meta for %s: %v\n", transcriptPath, err)
		return ""
	}
	return meta.WorkflowPhase
}

// tailBytes bounds the common-case read: the latest usage record sits near EOF of an append-only file.
const tailBytes = 256 << 10

// Usage is the newest assistant usage record: total context tokens and the model that produced it.
type Usage struct {
	Tokens int
	Model  string
}

// LastUsage scans the tail back-to-front, falling back to a full scan when the tail holds no usage line.
func LastUsage(path string) (Usage, bool) {
	f, err := os.Open(path)
	if err != nil || path == "" {
		return Usage{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Usage{}, false
	}
	off := max(fi.Size()-tailBytes, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return Usage{}, false
	}
	for end := len(buf); end > 0; {
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		if u, ok := usageFromLine(buf[start:end]); ok {
			return u, true
		}
		end = start - 1
	}
	if off == 0 {
		return Usage{}, false
	}
	var last Usage
	found := false
	_ = EachLine(f, func(line []byte) {
		if u, ok := usageFromLine(line); ok {
			last, found = u, true
		}
	})
	return last, found
}

var usageKey = []byte(`"usage"`)

func usageFromLine(line []byte) (Usage, bool) {
	if !bytes.Contains(line, usageKey) {
		return Usage{}, false
	}
	var rec struct {
		IsSidechain bool `json:"isSidechain"`
		Message     struct {
			Model string `json:"model"`
			Usage *struct {
				Input         int `json:"input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
				Output        int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil || rec.Message.Usage == nil || rec.IsSidechain {
		return Usage{}, false
	}
	u := rec.Message.Usage
	return Usage{Tokens: u.Input + u.CacheRead + u.CacheCreation + u.Output, Model: rec.Message.Model}, true
}

// Window is the context window for a session model: 1M when usage already exceeds 200K or the launch
// env pins a [1m] variant of the model (transcripts record the wire id, never the marker).
func Window(model string, used int) int {
	if used > models.ContextWindowStd {
		return models.ContextWindow1M
	}
	if models.Has1M(model) {
		return models.ContextWindow1M
	}
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		v := os.Getenv(k)
		// Databricks pins "databricks-<id>" but the transcript records the bare id, hence the prefix strip.
		base := models.Strip1M(v)
		if model != "" && (base == model || strings.HasSuffix(base, ":"+model) ||
			strings.TrimPrefix(base, models.DatabricksPrefix) == model) {
			return models.ContextWindow(v)
		}
	}
	return models.ContextWindow(model)
}

// windowFile holds the context window Claude Code reported for a session, written by the status line.
func windowFile(session string) string {
	return filepath.Join(paths.StateDir(), "context", filepath.Base(session))
}

// RecordWindow stores the window Claude Code reports for session; the file is rewritten only when the size changes.
func RecordWindow(session string, size int) {
	if session == "" || size <= 0 || RecordedWindow(session) == size {
		return
	}
	path := windowFile(session)
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = paths.AtomicWrite(path, []byte(strconv.Itoa(size)), 0o600)
	}
}

// RecordedWindow is the window RecordWindow stored for session, 0 when none: Claude Code's own figure beats a guess.
func RecordedWindow(session string) int {
	if session == "" {
		return 0
	}
	b, err := os.ReadFile(windowFile(session))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return max(n, 0)
}
