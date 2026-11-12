// provider_quotas.go — live, account-wide plan quota per provider: GitHub Copilot (every gh login), OpenAI (codex's
// ChatGPT sign-in), Google (Antigravity's agy CLI `/usage`). Tokens stay in memory and child env, never printed.

package usage

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/network/httpx"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/tokenledger"
	"github.com/edotau/claude-code/internal/ui/table"
)

// quota is one provider-account-window row; Note replaces the numbers on failure, Group labels a valid row.
type quota struct {
	Provider string  `json:"provider"`
	Account  string  `json:"account,omitempty"`
	Plan     string  `json:"plan,omitempty"`
	Window   string  `json:"window,omitempty"`
	UsedPct  float64 `json:"used_percent"`
	LeftPct  float64 `json:"left_percent"`
	Used     float64 `json:"used_credits,omitempty"`
	Limit    float64 `json:"limit_credits,omitempty"`
	Resets   string  `json:"resets,omitempty"`
	ResetsAt string  `json:"resets_at,omitempty"`
	Note     string  `json:"note,omitempty"`
	State    string  `json:"state,omitempty"`
	Group    string  `json:"group,omitempty"`
	Unit     string  `json:"unit,omitempty"` // "USD" renders Used/Limit as dollars; empty means credits
	// TokenExpiresAt is the Databricks auth token's expiry (RFC3339 UTC), absent when none is recorded.
	TokenExpiresAt string `json:"token_expires_at,omitempty"`
	AuthMethod     string `json:"auth_method,omitempty"` // the session profile's auth type: oauth, m2m or pat
	cached         bool   // served from a prior fetch's cache; never recorded as a fresh observation
}

// warnThreshold parses HARNESS_USAGE_WARN_PCT as float in (0,100], default 80; invalid → 80.
func warnThreshold() float64 {
	v := os.Getenv("HARNESS_USAGE_WARN_PCT")
	if v == "" {
		return 80
	}
	pct, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(pct) || pct <= 0 || pct > 100 {
		return 80
	}
	return pct
}

// noteOnly is true for an "unavailable" placeholder; a "cached; ..." row still carries a real UsedPct.
func noteOnly(q quota) bool {
	return q.Note != "" && q.Limit == 0 && !strings.HasPrefix(q.Note, "cached")
}

// setStates classifies each quota as "ok", "warn", or "exceeded" based on UsedPct and threshold.
func setStates(qs []quota, threshold float64) {
	for i := range qs {
		if noteOnly(qs[i]) {
			continue
		}
		switch {
		case qs[i].UsedPct >= 100:
			qs[i].State = "exceeded"
		case qs[i].UsedPct >= threshold:
			qs[i].State = "warn"
		default:
			qs[i].State = "ok"
		}
	}
}

// fetchQuotas is the network seam; tests replace it so no test reaches gh, chatgpt.com or agy.
var fetchQuotas = func() []quota {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	agyCtx, agyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer agyCancel()
	budgetCtx, budgetCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer budgetCancel()
	var an, gh, oa, ag, bud []quota
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); bud = sessionBudgetQuotas(budgetCtx) }()
	go func() { defer wg.Done(); an = claudeQuotas(ctx) }()
	go func() { defer wg.Done(); gh = githubQuotas(ctx) }()
	go func() { defer wg.Done(); oa = openaiQuotas(ctx) }()
	go func() { defer wg.Done(); ag = antigravityQuotas(agyCtx) }()
	wg.Wait()
	return append(append(append(append(an, gh...), oa...), ag...), bud...)
}

// ghRun runs gh with extra env; stdout only, so gh's stderr chatter never lands in a parse.
func ghRun(ctx context.Context, env []string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "gh", args...)
	c.Env = append(os.Environ(), env...)
	return c.Output()
}

var ghAccountRE = regexp.MustCompile(`Logged in to github\.com account (\S+)`)

// githubQuotas reads /copilot_internal/user once per gh login — personal and work seats bill separately.
func githubQuotas(ctx context.Context) []quota {
	c := exec.CommandContext(ctx, "gh", "auth", "status", "--hostname", "github.com")
	status, _ := c.CombinedOutput() // exits 1 when any account's token is stale; the others still list
	logins := ghAccountRE.FindAllSubmatch(status, -1)
	out := make([]quota, len(logins))
	var wg sync.WaitGroup
	for i, m := range logins {
		wg.Go(func() { out[i] = githubQuota(ctx, string(m[1])) })
	}
	wg.Wait()
	if len(out) == 0 {
		out = append(out, quota{Provider: "github", Note: "unavailable: no gh login"})
	}
	return out
}

// githubQuota reads one gh login's Copilot seat; the login's own token, never another account's.
func githubQuota(ctx context.Context, login string) quota {
	tok, err := ghRun(ctx, nil, "auth", "token", "--hostname", "github.com", "--user", login)
	if err != nil {
		return quota{Provider: "github", Account: login, Note: "unavailable: no token"}
	}
	body, err := ghRun(ctx, []string{"GH_TOKEN=" + strings.TrimSpace(string(tok))}, "api", "/copilot_internal/user")
	if err != nil {
		return quota{Provider: "github", Account: login, Note: "unavailable: no Copilot seat or API error"}
	}
	return parseCopilot(login, body)
}

// NoPremiumCredits is the note for a seat GitHub says holds no premium credits; it records no reading, never 100%.
const NoPremiumCredits = "no Copilot premium credits"

// parseCopilot turns a /copilot_internal/user body into its premium-credit row.
func parseCopilot(login string, body []byte) quota {
	var u struct {
		Plan  string `json:"copilot_plan"`
		SKU   string `json:"access_type_sku"`
		Reset string `json:"quota_reset_date"`
		Snaps struct {
			Premium struct {
				Entitlement float64  `json:"entitlement"`
				Used        float64  `json:"credits_used"`
				PctLeft     *float64 `json:"percent_remaining"`
				Unlimited   bool     `json:"unlimited"`
				HasQuota    *bool    `json:"has_quota"`
			} `json:"premium_interactions"`
		} `json:"quota_snapshots"`
	}
	q := quota{Provider: "github", Account: login, Window: "monthly"}
	if err := json.Unmarshal(body, &u); err != nil {
		q.Note = "unavailable: unreadable response"
		return q
	}
	p := u.Snaps.Premium
	q.Plan, q.Resets = providers.CopilotPlanName(u.Plan), u.Reset
	if p.Unlimited {
		q.Note = "unlimited"
		return q
	}
	if p.Entitlement == 0 {
		q.Note = "no premium quota reported"
		if p.HasQuota != nil && !*p.HasQuota { // GitHub says so outright: a definite no, still not a reading
			q.Note = NoPremiumCredits
			if strings.HasPrefix(u.SKU, "free") {
				q.Note += " (Free plan)"
			}
		}
		return q
	}
	pctLeft := 100 - 100*p.Used/p.Entitlement
	if p.PctLeft != nil {
		pctLeft = *p.PctLeft
	}
	q.UsedPct, q.Used, q.Limit = math.Round((100-pctLeft)*10)/10, p.Used, p.Entitlement
	return q
}

// whamURL is ChatGPT's plan-usage endpoint codex itself polls.
const whamURL = "https://chatgpt.com/backend-api/wham/usage"

// openaiQuotas reads codex's ChatGPT rate-limit windows (5h + weekly).
func openaiQuotas(ctx context.Context) []quota {
	tok, acct, keyringNote := codex.CodexAccessToken()
	if tok == "" {
		note := "unavailable: codex not signed in to ChatGPT"
		if keyringNote != "" {
			note += " (keyring: " + keyringNote + ")"
		}
		return []quota{{Provider: "openai", Note: note}}
	}
	status, body, _, err := httpx.Do(ctx, httpx.Call{Client: http.DefaultClient, URL: whamURL, Limit: 1 << 20,
		Header: http.Header{"Authorization": {"Bearer " + tok}, "Chatgpt-Account-Id": {acct}}})
	if status == 0 {
		return []quota{{Provider: "openai", Note: "unavailable: network error"}}
	}
	if status != http.StatusOK {
		return []quota{{Provider: "openai", Note: fmt.Sprintf("unavailable: HTTP %d (run codex to refresh login)", status)}}
	}
	if err != nil {
		return []quota{{Provider: "openai", Note: "unavailable: read error: " + textutil.FirstLine(err.Error())}}
	}
	return parseWham(body)
}

// parseWham turns a wham/usage body into one row per rate-limit window.
func parseWham(body []byte) []quota {
	type window struct {
		UsedPct float64 `json:"used_percent"`
		Seconds int64   `json:"limit_window_seconds"`
		ResetAt int64   `json:"reset_at"`
	}
	var u struct {
		Email string `json:"email"`
		Plan  string `json:"plan_type"`
		Rate  struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return []quota{{Provider: "openai", Note: "unavailable: unreadable response"}}
	}
	var out []quota
	for _, w := range []*window{u.Rate.Primary, u.Rate.Secondary} {
		if w == nil {
			continue
		}
		out = append(out, quota{Provider: "openai", Account: u.Email, Plan: textutil.Title(u.Plan), Window: windowLabel(w.Seconds),
			UsedPct: w.UsedPct, Resets: time.Unix(w.ResetAt, 0).Local().Format("2006-01-02 15:04")})
	}
	if len(out) == 0 {
		out = append(out, quota{Provider: "openai", Account: u.Email, Plan: textutil.Title(u.Plan), Note: "no rate-limit windows reported"})
	}
	return out
}

// windowLabel names a rate-limit window length: 18000 → "5h", 604800 → "weekly".
func windowLabel(sec int64) string {
	switch {
	case sec == 7*24*3600:
		return "weekly"
	case sec%(24*3600) == 0 && sec > 0:
		return fmt.Sprintf("%dd", sec/(24*3600))
	default:
		return fmt.Sprintf("%dh", sec/3600)
	}
}

// bar generates a 10-cell filled/empty bar for a percentage (0-100).
func bar(pct float64) string {
	capped := int(math.Round(math.Min(math.Max(pct, 0), 100)))
	filled := (capped * 10) / 100
	if filled == 0 && capped > 0 {
		filled = 1
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
}

// printQuotas renders the subscription quota table (skipped for --show total) and the API-key ledger
// section below the transcript table.
func printQuotas(out io.Writer, qs []quota, show string, aggs []ledgerAgg, since time.Time) {
	if show != "total" {
		printSubscriptionQuotas(out, qs, show)
	}
	printLedgerSection(out, aggs, since)
}

// printSubscriptionQuotas renders one row per provider-account-window; column and value follow show.
func printSubscriptionQuotas(out io.Writer, qs []quota, show string) {
	if len(qs) == 0 {
		return
	}
	col := "LEFT"
	if show == "used" {
		col = "USAGE"
	}
	fmt.Fprintf(out, "\nPROVIDER QUOTAS (live, account-wide)\n")
	head := []string{"PROVIDER", "ACCOUNT", "PLAN", "WINDOW", col, "STATE", "RESETS", "DETAIL"}
	var rows [][]string
	for _, q := range qs {
		pct, detail := q.UsedPct, q.Note
		if q.Group != "" {
			detail = q.Group
		}
		if show != "used" {
			pct = q.LeftPct
		}
		val, state := bar(pct)+" "+fmt.Sprintf("%4s", fmt.Sprintf("%.0f%%", pct)), q.State
		if noteOnly(q) {
			val, state = "-", "-"
		}
		if q.Limit > 0 {
			detail = limitDetail(q, show)
		}
		rows = append(rows, []string{q.Provider, dash(q.Account), dash(q.Plan), dash(q.Window), val, state, dash(q.Resets), detail})
	}
	fmt.Fprint(out, table.Render(head, nil, rightAlign(head, rows, 5, 5), 0))
}

// limitDetail renders a limited quota's used-of-limit text in dollars or credits, as used or as left.
func limitDetail(q quota, show string) string {
	left := max(q.Limit-q.Used, 0)
	switch {
	case q.Unit == "USD" && show == "used":
		return fmt.Sprintf("$%.2f / $%.2f", q.Used, q.Limit)
	case q.Unit == "USD":
		return fmt.Sprintf("$%.2f of $%.2f left", left, q.Limit)
	case show == "used":
		return fmt.Sprintf("%.0f / %.0f credits", q.Used, q.Limit)
	}
	return fmt.Sprintf("%.0f of %.0f credits left", left, q.Limit)
}

// printLedgerSection renders the local per-token ledger rollup; an empty ledger prints one line instead.
func printLedgerSection(out io.Writer, aggs []ledgerAgg, since time.Time) {
	fmt.Fprintf(out, "\nAPI KEYS (per-token, accumulated via harness)\n")
	if len(aggs) == 0 {
		when := "now"
		if !since.IsZero() {
			when = since.Format("2006-01-02")
		}
		fmt.Fprintf(out, "no per-token calls recorded yet (ledger started %s)\n", when)
		return
	}
	head := []string{"PROVIDER", "MODEL", "TODAY", "30D", "ALL-TIME"}
	var rows [][]string
	for _, a := range aggs {
		rows = append(rows, []string{rowLabel(a.Provider, a.Profile), dash(a.Model), humanTokens(a.Today), humanTokens(a.Last30d), humanTokens(a.AllTime)})
	}
	fmt.Fprint(out, table.Render(head, nil, rightAlign(head, rows, 2, 4), 0))
}

func dash(s string) string { return cmp.Or(s, "-") }

// agyUsageCmd is the agy exec seam; tests replace it with a fake binary so none of this reaches agy.
var agyUsageCmd = func(ctx context.Context) ([]byte, error) {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, "-p", "/usage", "--output-format", "json", "--print-timeout", "20s").Output()
}

// agyUsageBucket is one rate-limit bucket inside an agy usage group; disabled buckets carry no numbers.
type agyUsageBucket struct {
	Window        string  `json:"window"`
	RemainingFrac float64 `json:"remaining_fraction"`
	ResetTime     string  `json:"reset_time"`
	Disabled      bool    `json:"disabled"`
}

// agyUsageGroup is one named group of buckets ("Gemini Models", "Claude and GPT models", ...).
type agyUsageGroup struct {
	Name    string           `json:"name"`
	Buckets []agyUsageBucket `json:"buckets"`
}

// agyUsageResult mirrors agy -p "/usage" --output-format json: one top-level status plus the usage command.
type agyUsageResult struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Groups []agyUsageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// antigravityQuotas reads agy's `/usage` output: one quota row per enabled bucket, "google"/"Antigravity".
func antigravityQuotas(ctx context.Context) []quota {
	out, err := agyUsageCmd(ctx)
	if err != nil && len(out) == 0 {
		if _, lookErr := paths.AgentBin("agy"); lookErr != nil {
			return agyNote("agy not installed")
		}
		return agyNote("agy usage failed: " + commandErrorLine(out, err))
	}
	if strings.Contains(string(out), "not logged in") || strings.Contains(string(out), "run agy login") {
		return agyNote("agy not logged in (run agy login)")
	}
	if i := strings.Index(string(out), "AGY_ERROR:"); i >= 0 {
		return agyNote("agy usage failed: " + commandErrorLine(out[i:], err))
	}
	res, findErr := findAgyUsageResult(out)
	if findErr != nil {
		return agyNote("agy usage failed: " + findErr.Error())
	}
	return agyUsageRows(res)
}

// agyNote is the single google row shown when agy can't report numbers.
func agyNote(note string) []quota {
	return []quota{{Provider: "google", Plan: "Antigravity", Note: note}}
}

// agyUsageRows turns every enabled bucket into one quota row, detail carrying the bucket's group label.
func agyUsageRows(res agyUsageResult) []quota {
	var out []quota
	for _, g := range res.Command.Data.Groups {
		for _, b := range g.Buckets {
			if b.Disabled {
				continue
			}
			resets, resetsAt := claudeResets(b.ResetTime)
			out = append(out, quota{
				Provider: "google", Plan: "Antigravity", Account: "-",
				Window:   b.Window,
				UsedPct:  math.Round((1 - b.RemainingFrac) * 100),
				Resets:   resets,
				ResetsAt: resetsAt,
				Group:    g.Name,
			})
		}
	}
	if len(out) == 0 {
		return agyNote("agy usage reported no buckets")
	}
	return out
}

// findAgyUsageResult scans stdout line by line for the first JSON object naming the usage command.
func findAgyUsageResult(out []byte) (agyUsageResult, error) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var res agyUsageResult
		if err := json.Unmarshal(line, &res); err != nil {
			continue
		}
		if res.Status == "SUCCESS" && res.Command.Name == "usage" {
			return res, nil
		}
	}
	return agyUsageResult{}, errors.New("no successful usage result in agy output")
}

// commandErrorLine trims a command's combined error to one line, capped at 80 chars, never the whole stream.
func commandErrorLine(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		s = err.Error()
	}
	s, _, _ = strings.Cut(textutil.FirstLine(s), "\r")
	return textutil.TruncateRunes(s, 80)
}

// ledgerAgg is one provider+profile+model row for the API KEYS section: tokens over today / 30d / all-time.
type ledgerAgg struct {
	Provider string `json:"provider"` // the platform
	Profile  string `json:"profile,omitempty"`
	Model    string `json:"model"`
	Today    int    `json:"today"`
	Last30d  int    `json:"last_30d"`
	AllTime  int    `json:"all_time"`
}

// aggregateLedger rolls entries up per provider+profile+model, sorted by provider, profile, then model.
func aggregateLedger(entries []tokenledger.Entry, now time.Time) []ledgerAgg {
	type key struct{ provider, profile, model string }
	byKey := map[key]*ledgerAgg{}
	var order []key
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()) // local midnight, not UTC
	cutoff30 := now.Add(-30 * 24 * time.Hour)
	for _, e := range entries {
		k := key{e.Provider, e.ProfileName(), e.Model}
		a, ok := byKey[k]
		if !ok {
			a = &ledgerAgg{Provider: e.Provider, Profile: k.profile, Model: e.Model}
			byKey[k] = a
			order = append(order, k)
		}
		total := e.Input + e.Output + e.CacheRead
		a.AllTime += total
		if !e.Ts.Before(cutoff30) {
			a.Last30d += total
		}
		if !e.Ts.Before(today) {
			a.Today += total
		}
	}
	slices.SortFunc(order, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.profile, b.profile), cmp.Compare(a.model, b.model))
	})
	out := make([]ledgerAgg, len(order))
	for i, k := range order {
		out[i] = *byKey[k]
	}
	return out
}

// ledgerSince is the earliest entry's timestamp, used for the "ledger started <date>" empty message.
func ledgerSince(entries []tokenledger.Entry) time.Time {
	var since time.Time
	for _, e := range entries {
		if since.IsZero() || e.Ts.Before(since) {
			since = e.Ts
		}
	}
	return since
}

// humanTokens renders a token count compactly: 1.2M, 34k, 512.
func humanTokens(n int) string {
	switch {
	case n >= 999_500: // %.0fk would round up to "1000k"
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

// rowLabel is "<provider>·<profile>", the provider alone when the profile is empty or bears its name.
func rowLabel(provider, profile string) string {
	if provider == "" {
		return profile
	}
	if profile == "" || profile == provider {
		return provider
	}
	return provider + "·" + profile
}

// recordQuotaState stores the fetched rows in quota.json; a var so package tests never write the real file.
var recordQuotaState = func(qs []quota) { recordQuotas(qs, time.Now()) }

// recordQuotas keeps each account's worst window (Fable's model cap and failures excluded); returns the stored count.
func recordQuotas(qs []quota, now time.Time) int {
	worst := map[string]quota{}
	for _, q := range qs {
		key := accountKey(q)
		if key == "" || !recordable(q) {
			continue
		}
		if w, ok := worst[key]; !ok || q.UsedPct > w.UsedPct {
			worst[key] = q
		}
	}
	n := 0
	for key, q := range worst {
		if router.Record(key, q.UsedPct, q.Window, resetTime(q), now) == nil {
			n++
		}
	}
	return n
}

// recordable is true for a windowed, ungrouped, freshly fetched row; cached rows carry no fetch time.
func recordable(q quota) bool {
	return q.Window != "" && q.Group == "" && q.Note == "" && !q.cached
}

// accountKey maps a quota row to route's account key; "" for providers the router has no column for.
func accountKey(q quota) string {
	switch q.Provider {
	case "anthropic":
		return "anthropic·ultra"
	case "openai":
		return "openai·chatgpt"
	case "github":
		if q.Account != "" {
			reg, err := providers.Load()
			if err != nil {
				return "" // an unreadable registry would key the row by raw login; record nothing instead
			}
			return "github·" + providers.GithubAccountName(reg, q.Account)
		}
	}
	return ""
}

// resetTime reads a row's reset: the UTC instant when set, else the local display forms.
func resetTime(q quota) time.Time {
	if t, err := time.Parse(time.RFC3339, q.ResetsAt); err == nil {
		return t
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, q.Resets, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// RefreshAccount re-fetches one account (3 s; Anthropic's 60 s cache reused) and records it; false on no data.
// A fetch that records nothing stamps the account checked, so picks back off for StaleAfter instead of re-fetching.
func RefreshAccount(parent context.Context, key string) bool {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var qs []quota
	switch {
	case key == "anthropic·ultra":
		qs = claudeQuotas(ctx)
	case key == "openai·chatgpt":
		qs = openaiQuotas(ctx)
	case strings.HasPrefix(key, "github·"):
		reg, err := providers.Load()
		if err != nil {
			return false
		}
		qs = []quota{githubQuota(ctx, providers.GithubAccountLogin(reg, strings.TrimPrefix(key, "github·")))}
	default:
		return false
	}
	setStates(qs, warnThreshold())
	now := time.Now()
	if recordQuotas(qs, now) > 0 {
		return true
	}
	if parent.Err() == nil { // the caller's own deadline cut the fetch short: no verdict to back off on
		_ = router.MarkChecked(key, checkNote(qs), now)
	}
	return false
}

// checkNote is the first row's note, else why nothing recordable came back.
func checkNote(qs []quota) string {
	for _, q := range qs {
		if q.Note != "" {
			return q.Note
		}
	}
	return "no recordable window"
}
