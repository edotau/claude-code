package transcript

import (
	"encoding/json"
	"os"
	"time"
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

func (m *ModelUsage) add(o ModelUsage) {
	m.InputTokens += o.InputTokens
	m.OutputTokens += o.OutputTokens
	m.CacheReadTokens += o.CacheReadTokens
	m.CacheCreationTokens += o.CacheCreationTokens
}

// FileSummary is one transcript: per-model totals, API responses (Turns) and the timestamp span.
type FileSummary struct {
	Models map[string]*ModelUsage
	Turns  int
	First  time.Time // zero when no counted record had an RFC3339 timestamp
	Last   time.Time
	perMin map[int64]int64 // unix minute → input tokens, feeds the aggregate's peak
}

// Totals sums every model in the file.
func (s *FileSummary) Totals() ModelUsage {
	var t ModelUsage
	for _, m := range s.Models {
		t.add(*m)
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

type response struct {
	model string
	usage ModelUsage
	ts    string
}

// SummarizeFile counts each API response once: Claude Code writes one line per content block, each repeating
// the response's usage under one message.id, so the last line per id wins (output grows while streaming).
func SummarizeFile(path string) (*FileSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byID := map[string]int{}
	var resps []response
	eachLine(f, func(line []byte) {
		var rec usageLine
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.Usage == nil {
			return
		}
		u := rec.Message.Usage
		r := response{rec.Message.Model, ModelUsage{u.InputTokens, u.OutputTokens, u.CacheReadInput, u.CacheCreationInput}, rec.Timestamp}
		if i, seen := byID[rec.Message.ID]; seen && rec.Message.ID != "" {
			resps[i] = r
			return
		}
		byID[rec.Message.ID] = len(resps)
		resps = append(resps, r)
	})
	s := &FileSummary{Models: map[string]*ModelUsage{}, Turns: len(resps), perMin: map[int64]int64{}}
	for _, r := range resps {
		s.count(r)
	}
	return s, nil
}

func (s *FileSummary) count(r response) {
	m := s.Models[r.model]
	if m == nil {
		m = &ModelUsage{}
		s.Models[r.model] = m
	}
	m.add(r.usage)
	ts, err := time.Parse(time.RFC3339, r.ts)
	if err != nil {
		return
	}
	// ITPM counts uncached input: fresh input plus cache writes; cache reads don't count on current models.
	s.perMin[ts.Truncate(time.Minute).Unix()] += r.usage.InputTokens + r.usage.CacheCreationTokens
	if s.First.IsZero() || ts.Before(s.First) {
		s.First = ts
	}
	if ts.After(s.Last) {
		s.Last = ts
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
		m.add(*u)
	}
	for minute, n := range s.perMin {
		a.perMin[minute] += n
		a.PeakInputTokensPerMin = max(a.PeakInputTokensPerMin, a.perMin[minute])
	}
}

// AddFile summarizes path and folds it in.
func (a *UsageAggregate) AddFile(path string) error {
	s, err := SummarizeFile(path)
	if err != nil {
		return err
	}
	a.Add(s)
	return nil
}
