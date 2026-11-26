package agent

import (
	"context"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/router/jev"
)

// JevShadow is what Open-Jev would pick for a dispatch's brief beside TaskTier's keyword pick; it routes nothing.
type JevShadow struct {
	Pick       string  `json:"pick,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Ms         int64   `json:"ms,omitempty"`
	Agent      string  `json:"agent,omitempty"` // Open-Jev's agent type pick
	AgentConf  float64 `json:"agent_conf,omitempty"`
	Helper     string  `json:"helper,omitempty"`  // local Qwen's tier, asked only when Open-Jev is unsure
	Final      string  `json:"final,omitempty"`   // Open-Jev if confident, else fast when Qwen says fast, else keyword
	Keyword    string  `json:"keyword,omitempty"` // TaskTier's pick for the same brief: fast | standard | deep
	Error      string  `json:"error,omitempty"`   // down | timeout | bad-response | off
}

type jevCall struct {
	keyword string
	done    chan JevShadow
	cancel  context.CancelFunc
}

var jevPending sync.Map // record id -> *jevCall, taken by Record.Finish

// startJev asks Open-Jev about prompt in the background so the launch never waits on it.
func startJev(ctx context.Context, id, prompt string) {
	text := jevBrief(prompt)
	if id == "" || text == "" {
		return
	}
	call := &jevCall{keyword: tierName(prompt), done: make(chan JevShadow, 1)}
	ctx, call.cancel = context.WithCancel(ctx) // the shadow dies with the dispatch, and takeJev/stopJev stop it sooner
	jevPending.Store(id, call)
	go func() {
		res, err := jev.Tier(ctx, text)
		j := JevShadow{Pick: res.Pick, Confidence: res.Confidence, Ms: res.Ms, Agent: res.Agent, AgentConf: res.AgentConfidence, Keyword: call.keyword, Error: jev.Class(err)}
		if res.Confidence < jev.Confident {
			j.Helper, _ = jev.Helper(ctx, text)
		}
		j.Final = j.final()
		call.done <- j
	}()
}

// takeJev is the shadow result for id (timeout if the run ended first); nil when none was started.
func takeJev(id string) *JevShadow {
	v, ok := jevPending.LoadAndDelete(id)
	if !ok {
		return nil
	}
	call := v.(*jevCall)
	call.cancel() // the record is written: an unanswered shadow stops its call
	select {
	case j := <-call.done:
		return &j
	default:
		return &JevShadow{Keyword: call.keyword, Final: call.keyword, Error: jev.ClassTimeout}
	}
}

// stopJev drops id's pending shadow and cancels its call; the parent of a respawned job never takes it.
func stopJev(id string) {
	if v, ok := jevPending.LoadAndDelete(id); ok {
		v.(*jevCall).cancel()
	}
}

// tierName is TaskTier's pick for brief as a tier name ("" for none).
func tierName(brief string) string {
	alias, _ := TaskTier(brief)
	for name, a := range tierAliases {
		if a == alias {
			return name
		}
	}
	return ""
}

// JevPair is Open-Jev's shadow answer to "does B need A's result first?"; it changes no dispatch order.
type JevPair struct {
	A          int     `json:"a"`
	B          int     `json:"b"`
	Needs      string  `json:"needs,omitempty"` // yes | no
	Confidence float64 `json:"confidence,omitempty"`
	Ms         int64   `json:"ms,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// JevPairs asks about every ordered pair (i<j) of tasks, one call each, in order.
func JevPairs(ctx context.Context, tasks []string) []JevPair {
	var out []JevPair
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			res, err := jev.Needs(ctx, tasks[i], tasks[j])
			out = append(out, JevPair{A: i, B: j, Needs: res.Pick, Confidence: res.Confidence, Ms: res.Ms, Error: jev.Class(err)})
		}
	}
	return out
}

// jevBrief is the part of a dispatch prompt Open-Jev judges: no review-loop goal, leaf rules or boilerplate.
func jevBrief(prompt string) string {
	text, _, _ := strings.Cut(WithoutReviewLoop(prompt), "\n## Leaf rules")
	return strings.TrimSpace(boilerRe().ReplaceAllString(text, ""))
}

// final is the pick R5's rule would act on: a confident Open-Jev, else fast on Qwen's say-so, else the keywords.
func (j JevShadow) final() string {
	switch {
	case j.Error == "" && j.Confidence >= jev.Confident:
		return j.Pick
	case j.Helper == "fast":
		return "fast"
	}
	return j.Keyword
}
