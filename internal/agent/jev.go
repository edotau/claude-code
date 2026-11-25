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
	text, _, _ := strings.Cut(WithoutReviewLoop(prompt), "\n## Leaf rules")
	text = strings.TrimSpace(boilerRe().ReplaceAllString(text, ""))
	if id == "" || text == "" {
		return
	}
	call := &jevCall{keyword: tierName(prompt), done: make(chan JevShadow, 1)}
	ctx, call.cancel = context.WithCancel(ctx) // the shadow dies with the dispatch, and takeJev/stopJev stop it sooner
	jevPending.Store(id, call)
	go func() {
		res, err := jev.Tier(ctx, text)
		call.done <- JevShadow{Pick: res.Pick, Confidence: res.Confidence, Ms: res.Ms, Keyword: call.keyword, Error: jev.Class(err)}
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
		return &JevShadow{Keyword: call.keyword, Error: jev.ClassTimeout}
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
