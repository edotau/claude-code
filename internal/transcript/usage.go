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
	"github.com/edotau/claude-code/internal/providers"
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

// ResponseAccumulator deduplicates transcript response lines by message id, retaining first-seen order.
type ResponseAccumulator struct {
	Responses []Response
	byID      map[string]int
}

func (a *ResponseAccumulator) Add(line []byte) {
	if !bytes.Contains(line, usageKey) { // a line without a "usage" key cannot count; skips the full decode
		return
	}
	var rec usageLine
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.Usage == nil {
		return
	}
	u := rec.Message.Usage
	r := Response{Model: rec.Message.Model, Usage: ModelUsage{u.InputTokens, u.OutputTokens, u.CacheReadInput, u.CacheCreationInput}}
	r.At, _ = time.Parse(time.RFC3339, rec.Timestamp)
	if i, seen := a.byID[rec.Message.ID]; seen && rec.Message.ID != "" {
		a.Responses[i] = r
		return
	}
	if a.byID == nil {
		a.byID = map[string]int{}
	}
	a.byID[rec.Message.ID] = len(a.Responses)
	a.Responses = append(a.Responses, r)
}

// ReadResponses lists each API response once, since one message.id repeats across content-block lines (last wins).
func ReadResponses(path string) ([]Response, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var acc ResponseAccumulator
	if err := EachLine(f, acc.Add); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return acc.Responses, nil
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

// UsageAggregate totals many transcripts; PeakInputTokensPerMin (peak one-minute uncached input) is the 429-risk proxy.
type UsageAggregate struct {
	Models                map[string]*ModelUsage `json:"models"`
	PeakInputTokensPerMin int64                  `json:"peak_input_tokens_per_min"`
	ModelTurns            map[string]int         `json:"-"` // model id -> response count across every added file

	perMin map[int64]int64
}

// NewUsageAggregate returns an empty aggregate.
func NewUsageAggregate() *UsageAggregate {
	return &UsageAggregate{Models: map[string]*ModelUsage{}, ModelTurns: map[string]int{}, perMin: map[int64]int64{}}
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
		a.ModelTurns[model] += s.ModelTurns[model]
	}
	for minute, n := range s.perMin {
		a.perMin[minute] += n
		a.PeakInputTokensPerMin = max(a.PeakInputTokensPerMin, a.perMin[minute])
	}
}

// GlobLiteral escapes s's glob metacharacters as one-rune classes, which filepath.Match reads literally on every OS.
var GlobLiteral = strings.NewReplacer(globEscapes()...).Replace

// globEscapes pairs each metacharacter with its class; `\` is an escape only where it is not the path separator.
func globEscapes() []string {
	pairs := []string{`*`, `[*]`, `?`, `[?]`, `[`, `[[]`}
	if filepath.Separator != '\\' {
		pairs = append(pairs, `\`, `[\\]`)
	}
	return pairs
}

// GlobIn is filepath.Glob(dir/pattern) with dir matched literally.
func GlobIn(dir, pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(GlobLiteral(dir), pattern))
	return m
}

// WorkflowRunDirs lists the subagents/workflows/<runId> dirs of a session dir.
func WorkflowRunDirs(sessionDir string) []string {
	var dirs []string
	for _, m := range GlobIn(filepath.Join(sessionDir, "subagents", "workflows"), "*") {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			dirs = append(dirs, m)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// SessionAgentFiles lists a session dir's subagent and workflow-agent transcripts; a path not on disk is a glob.
func SessionAgentFiles(sessionDir string) ([]string, error) {
	dirs := []string{sessionDir}
	var err error
	if _, serr := os.Stat(sessionDir); serr != nil {
		dirs, err = filepath.Glob(sessionDir)
	}
	var files []string
	for _, dir := range dirs {
		files = append(files, GlobIn(filepath.Join(dir, "subagents"), "*.jsonl")...)
		for _, run := range WorkflowRunDirs(dir) {
			files = append(files, WorkflowAgentFiles(run)...)
		}
	}
	return files, err
}

// WorkflowAgentFiles lists one run dir's agent-*.jsonl transcripts, keeping *.meta.json sidecars out of the scan.
func WorkflowAgentFiles(runDir string) []string {
	matches := GlobIn(runDir, "agent-*.jsonl")
	slices.Sort(matches)
	return matches
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
	if u, ok := usageInTail(buf); ok {
		return u, true
	}
	if off == 0 {
		return Usage{}, false
	}
	return usageFullScan(f)
}

// usageInTail returns the newest usage record in buf, scanning lines back-to-front.
func usageInTail(buf []byte) (Usage, bool) {
	for end := len(buf); end > 0; {
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		if u, ok := usageFromLine(buf[start:end]); ok {
			return u, true
		}
		end = start - 1
	}
	return Usage{}, false
}

// usageFullScan returns the last usage record in f, reading every line.
func usageFullScan(f *os.File) (Usage, bool) {
	var last Usage
	found := false
	if err := EachLine(f, func(line []byte) {
		if u, ok := usageFromLine(line); ok {
			last, found = u, true
		}
	}); err != nil {
		return Usage{}, false
	}
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
	if m := rec.Message.Model; m == "" || strings.HasPrefix(m, "<") { // "<synthetic>" records carry no real model
		return Usage{}, false
	}
	u := rec.Message.Usage
	return Usage{Tokens: u.Input + u.CacheRead + u.CacheCreation + u.Output, Model: rec.Message.Model}, true
}

// Window is the launched window of model (providers.Window); usage above 200K guesses 1M only when no model is known.
func Window(model string, used int) int {
	if model == "" && used > models.ContextWindowStd {
		return models.ContextWindow1M
	}
	p := providers.SessionProvider()
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		v := os.Getenv(k)
		// The pin may omit the "databricks-" prefix the transcript's wire id carries, hence the prefix strip.
		base := models.Strip1M(v)
		if model != "" && (base == model || strings.HasSuffix(base, ":"+model) ||
			strings.TrimPrefix(base, models.DatabricksPrefix) == model) {
			return providers.Window(p, v)
		}
	}
	return providers.Window(p, model)
}

// windowFile holds the status-line-reported window for a session; "" for an id that is not a plain file name.
func windowFile(session string) string {
	name := filepath.Base(session)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(paths.StateDir(), "context", name)
}

// RecordWindow stores the window Claude Code reports for session; the file is rewritten only when the size changes.
func RecordWindow(session string, size int) {
	path := windowFile(session)
	if path == "" || size <= 0 || RecordedWindow(session) == size {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = paths.AtomicWrite(path, []byte(strconv.Itoa(size)), 0o600)
	}
}

// RecordedWindow is the window RecordWindow stored for session, 0 when none: Claude Code's own figure beats a guess.
func RecordedWindow(session string) int {
	path := windowFile(session)
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return max(n, 0)
}
