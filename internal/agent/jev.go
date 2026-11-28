package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/jev"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// JevShadow is Open-Jev's answer for a dispatch's brief beside the keyword pick; Final is the tier TaskTier acts on.
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

// startJev records Open-Jev's answers for prompt in the background, sharing the calls TaskTier and JevAgent make.
func startJev(ctx context.Context, id, prompt string) {
	if id == "" || jevBrief(prompt) == "" {
		return
	}
	call := &jevCall{keyword: tierName(prompt), done: make(chan JevShadow, 1)}
	ctx, call.cancel = context.WithCancel(ctx) // the shadow dies with the dispatch, and takeJev/stopJev stop it sooner
	jevPending.Store(id, call)
	go func() {
		j, a := jevClassify(ctx, prompt)
		j.Agent, j.AgentConf, j.Final = a.Pick, a.Confidence, j.final()
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

// tierName is the keyword classifier's pick for brief as a tier name ("" for none).
func tierName(brief string) string {
	alias, _ := keywordTier(brief)
	for name, a := range tierAliases {
		if a == alias {
			return name
		}
	}
	return ""
}

var jevMemo sync.Map // sha256(kind, brief) -> func() T: concurrent askers share one call

// jevOnce asks once per (kind, brief) in this process; the first asker's context bounds the shared call.
func jevOnce[T any](kind, brief string, ask func() T) T {
	key := sha256.Sum256([]byte(kind + "\x00" + brief))
	f, _ := jevMemo.LoadOrStore(key, sync.OnceValue(ask))
	return f.(func() T)()
}

type jevClassification struct {
	ctx        context.Context
	agentReady chan struct{} // closed once Open-Jev has answered (agent is set)
	done       chan struct{} // closed after the optional helper too (shadow is set)
	shadow     JevShadow
	agent      jev.Result
}

// jevShared starts or joins the one request for brief; the first asker's context bounds it.
func jevShared(ctx context.Context, brief string) *jevClassification {
	return jevOnce("classify", brief, func() *jevClassification {
		c := &jevClassification{ctx: ctx, agentReady: make(chan struct{}), done: make(chan struct{})}
		go c.run(brief)
		return c
	})
}

func (c *jevClassification) run(brief string) {
	defer close(c.done)
	text := jevBrief(brief)
	res, a, err := jev.Classify(c.ctx, text)
	c.shadow = JevShadow{Pick: res.Pick, Confidence: res.Confidence, Ms: res.Ms, Keyword: tierName(brief), Error: jev.Class(err)}
	if err == nil {
		c.agent = a
	}
	close(c.agentReady) // the agent answer never waits on the optional helper
	if c.shadow.Error == "" && res.Confidence < jev.Confident {
		if row := providers.LastResort(loadRegistryWarn()); row != nil {
			c.shadow.Helper, _ = jev.Helper(c.ctx, row.BaseURL, row.Models["haiku"], text)
		}
	}
}

// askJevClassify runs one unshared classification to completion.
func askJevClassify(ctx context.Context, brief string) (JevShadow, jev.Result) {
	c := &jevClassification{ctx: ctx, agentReady: make(chan struct{}), done: make(chan struct{})}
	c.run(brief)
	return c.shadow, c.agent
}

// wait reports whether ch closed before ctx or the shared call's own context ended.
func (c *jevClassification) wait(ctx context.Context, ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
	}
	select {
	case <-ch:
		return true
	case <-ctx.Done():
	case <-c.ctx.Done():
	}
	return false
}

// jevClassify shares one request per brief; each waiter can leave on its own deadline.
func jevClassify(ctx context.Context, brief string) (JevShadow, jev.Result) {
	brief = WithoutReviewLoop(brief)
	c := jevShared(ctx, brief)
	if c.wait(ctx, c.done) {
		return c.shadow, c.agent
	}
	return JevShadow{Keyword: tierName(brief), Error: jev.ClassTimeout}, jev.Result{}
}

// jevTierAnswer is the shared tier answer and any low-confidence helper opinion.
func jevTierAnswer(ctx context.Context, brief string) JevShadow {
	j, _ := jevClassify(ctx, brief)
	return j
}

// jevAgentAnswer returns as soon as Open-Jev answers, without waiting for the optional helper.
func jevAgentAnswer(ctx context.Context, brief string) jev.Result {
	brief = WithoutReviewLoop(brief)
	c := jevShared(ctx, brief)
	if c.wait(ctx, c.agentReady) {
		return c.agent
	}
	return jev.Result{}
}

// JevAgent asks Open-Jev for brief's agent vendor when confident; "" falls back to the default agent.
func JevAgent(brief string) string {
	return JevAgentCtx(context.Background(), brief)
}

// JevAgentCtx bounds the shared classifier by the caller's deadline.
func JevAgentCtx(ctx context.Context, brief string) string {
	if jevBrief(brief) == "" {
		return ""
	}
	if res := jevAgentAnswer(ctx, brief); res.Confidence >= jev.Confident && agentid.Known(res.Pick) {
		return res.Pick
	}
	return ""
}

// jevTier is the shared tier pick; "" falls back to keywords.
func jevTier(brief string) (alias, signal string) {
	return jevTierCtx(context.Background(), brief)
}

func jevTierCtx(ctx context.Context, brief string) (alias, signal string) {
	if jevBrief(brief) == "" {
		return "", ""
	}
	tier, source := jevTierAnswer(ctx, brief).routed()
	if a := tierAliases[tier]; a != "" {
		return a, source + ":" + tier
	}
	return "", ""
}

// TaskTierCtx preserves explicit tier and keyword precedence while bounding the shared Jev call.
func TaskTierCtx(ctx context.Context, brief string) (alias, signal string) {
	brief = WithoutReviewLoop(brief)
	if tierLineRe().MatchString(brief) {
		return keywordTier(brief)
	}
	if alias, signal := jevTierCtx(ctx, brief); alias != "" {
		return alias, signal
	}
	return keywordTier(brief)
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

// jevPairSlots bounds concurrent pair calls: Jev and the local model share one GPU.
const jevPairSlots = 4

// JevPairs asks about every ordered pair (i<j) of tasks and returns the ones that dialed, in that order.
func JevPairs(ctx context.Context, tasks []string) []JevPair {
	var idx [][2]int
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			idx = append(idx, [2]int{i, j})
		}
	}
	type asked struct {
		pair JevPair
		ran  bool
	}
	// A down or off dial returns its error so Map cancels the rest: one failed dial, not N.
	out, _ := parallel.Map(ctx, idx, jevPairSlots, func(ctx context.Context, p [2]int) (asked, error) {
		if ctx.Err() != nil {
			return asked{}, nil // cancelled before dialing: not on the record, as in the serial early return
		}
		res, err := jev.Needs(ctx, tasks[p[0]], tasks[p[1]])
		pair := JevPair{A: p[0], B: p[1], Needs: res.Pick, Confidence: res.Confidence, Ms: res.Ms, Error: jev.Class(err)}
		if pair.Error != jev.ClassDown && pair.Error != jev.ClassOff {
			err = nil
		}
		return asked{pair: pair, ran: true}, err
	})
	var answered []JevPair
	for _, a := range out {
		if a.ran {
			answered = append(answered, a.pair)
		}
	}
	return answered
}

var (
	jevRecallRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)<memory-recall>.*?</memory-recall>`) })
	jevTailRe   = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\n\s*(?:## Leaf rules|Review loop|Before reporting)`)
	})
	jevMetaRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?m)^[ \t]*(?:Tier|Workdir|Owned|Verify|Report|HARD CAP):.*$`)
	})
	jevBlankRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\n{3,}`) })
)

// jevBrief mirrors clean_brief in the router's finetune_jev.py so Open-Jev sees what it trained on; code spans stay.
func jevBrief(prompt string) string {
	text := jevRecallRe().ReplaceAllString(prompt, "")
	if loc := jevTailRe().FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	text = jevMetaRe().ReplaceAllString(text, "")
	return strings.TrimSpace(jevBlankRe().ReplaceAllString(text, "\n\n"))
}

// routed is the tier a confident Open-Jev or the Qwen helper decides, and which did; a helper never turns deep to fast.
func (j JevShadow) routed() (tier, source string) {
	switch {
	case j.Error == "" && j.Confidence >= jev.Confident && j.Keyword == "deep" && j.Pick != "deep":
		return "", "" // the served checkpoint under-calls deep (2 of 18 real deep gold briefs right): keywords keep deep
	case j.Error == "" && j.Confidence >= jev.Confident:
		return j.Pick, "jev"
	case j.Helper == "" || j.Helper == "fast" && j.Keyword == "deep":
		return "", ""
	}
	return j.Helper, "jev-qwen"
}

// final is the tier TaskTier acts on: routed's, else the keywords'.
func (j JevShadow) final() string {
	tier, _ := j.routed()
	return cmp.Or(tier, j.Keyword)
}
