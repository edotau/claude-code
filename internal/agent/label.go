package agent

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// RunQuery is q as the run's first hop resolves it; a routed external dispatch takes the ladder's first hop.
func RunQuery(ctx context.Context, reg *providers.Registry, q ModelQuery, aliasHint string) (ModelQuery, string, error) {
	if q.Agent == agentid.Claude || q.Model != "" || q.Profile != "" || q.Provider != "" || q.Account != "" {
		return q, "", nil
	}
	if QueryFor(ctx, q).PinModel == router.DiscoveryModel { // the ladder would advance the rotation; a label must not
		return q, "a discovery pin picks its model at run time", nil
	}
	chain, _, err := Resolve(ctx, RouteReq{Agent: q.Agent, AliasHint: aliasHint, Fallback: q.Fallback, Pinned: true})
	if err != nil {
		return q, "", err
	}
	if len(chain.Dispatches) == 0 {
		return q, "", fmt.Errorf("no route runs %s (%s)", q.Agent, strings.Join(chain.Skipped, "; "))
	}
	return ladderHop(reg, q, chain.Dispatches[0])
}

// ladderHop is the query d's engine resolves: a `<row>:<model>` cell as typed, an alias cell the row's pin or tier.
func ladderHop(reg *providers.Registry, q ModelQuery, d Dispatch) (ModelQuery, string, error) {
	hop := q
	hop.Agent = d.Agent.Name()
	acct := d.Account
	if _, row, ok := strings.Cut(acct, "·"); ok {
		acct = row
	}
	if reg.Row(acct) != nil {
		hop.Profile = acct
	}
	if !d.keepCaller && !d.unroutedDispatch && (strings.Contains(d.Model, ":") || hop.Agent == agentid.Claude) {
		hop.Model = d.Model // dispatchRequest's CellModel: a bare alias is dropped for a vendor CLI's own pin
	}
	note := ""
	if hop.Agent != q.Agent {
		note = fmt.Sprintf("%s is gated out; the route runs %s", q.Agent, hop.Agent)
	}
	return hop, note, nil
}

// ResolveRun is ResolveModel with q.Default as the task tier; a tier the row cannot serve keeps the untiered pick.
func ResolveRun(reg *providers.Registry, q ModelQuery, now time.Time) (ModelRoute, error) {
	r, err := ResolveModel(reg, q, now)
	if q.Default == "" || q.Model != "" || err == nil && (q.PinModel != "" || tierServed(reg, r, now)) {
		return r, err
	}
	tier := q.Default
	q.Default = ""
	base, berr := ResolveModel(reg, q, now)
	if berr != nil {
		return r, err
	}
	why := fmt.Sprintf("task tier %s is not served on %s; running %s", tier, base.Profile, base.Model)
	base.Note = strings.Join(slices.DeleteFunc([]string{why, base.Note}, func(s string) bool { return s == "" }), "; ")
	return base, nil
}

// tierServed is false only when r's Databricks row has a fresh listing without r's model READY.
func tierServed(reg *providers.Registry, r ModelRoute, now time.Time) bool {
	p := reg.Row(r.Profile)
	if p == nil || p.Preset != providers.PresetDatabricks {
		return true
	}
	_, ready, fresh := router.ServedListed(p, r.Model, now)
	return !fresh || ready
}

// tierAliases maps a brief's `Tier:` word to the alias each row spells as its model of that tier.
var tierAliases = map[string]string{"fast": models.Haiku, "standard": models.Sonnet, "deep": models.Opus}

var (
	tierLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?mi)^[ \t]*Tier:[ \t]*(fast|standard|deep)\b`) })
	// deepTaskRe: audits, planning, security or safety rules, parsers, judgment calls and design go to the top tier.
	deepTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(audit\w*|security|safety|threat\w*|parser\w*|parsing|grammar|judg(?:e)?ment|` +
			`planning|design|redesign|architect\w*)\b`)
	})
	fastTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(typos?|renames?|one[- ]line(?:r)?|reword|wording|commit message|message[- ]only|moves?)\b`)
	})
	scopeTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(across|every|all|repo-wide|codebase|call ?sites|callers|everywhere|throughout)\b`)
	})
	fileTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`[\w.-]*\w\.(?:go|py|md|sh|js|ts|tsx|json|ya?ml|toml|sql|rs|java|rb|css|html|tmpl)\b`)
	})
	// boilerRe: a brief's header lines and code spans name commands and paths, not the work.
	boilerRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("(?m)^[ \t]*(?:Run|Owned|Mode|Verify|Report|Tier):.*$|`[^`\n]*`")
	})
)

// TaskTier is the model alias a brief's work needs: its `Tier:` line, else deep, fast or standard; "" for no text.
func TaskTier(brief string) (alias, signal string) {
	if m := tierLineRe().FindStringSubmatch(brief); m != nil {
		return tierAliases[strings.ToLower(m[1])], "Tier: " + strings.ToLower(m[1])
	}
	text, _, _ := strings.Cut(brief, "\n## Leaf rules") // agents brief's footer is the same on every leaf
	text = strings.TrimSpace(boilerRe().ReplaceAllString(text, ""))
	switch {
	case text == "":
		return "", ""
	case deepTaskRe().MatchString(text):
		return models.Opus, "deep:" + strings.ToLower(deepTaskRe().FindString(text))
	}
	if rule := fastTask(text); rule != "" {
		return models.Haiku, rule
	}
	return models.Sonnet, "standard"
}

// fastTask names why text is a short question or one mechanical edit on at most two files; "" when it is not.
func fastTask(text string) string {
	if tier, rule := router.EffortTierSignal(text); tier == models.EffortLow {
		return rule
	}
	files := map[string]bool{}
	for _, f := range fileTaskRe().FindAllString(text, -1) {
		files[f] = true
	}
	if len(text) < 1000 && len(files) <= 2 && fastTaskRe().MatchString(text) && !scopeTaskRe().MatchString(text) {
		return "mechanical"
	}
	return ""
}
