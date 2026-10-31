// providers.go — live, account-wide plan quota per provider: GitHub Copilot (every gh login), OpenAI (codex's
// ChatGPT sign-in), Google (Antigravity's agy CLI `/usage`). Tokens stay in memory and child env, never printed.
package usage

import (
	"bufio"
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
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
}

// googleNote: AI Studio keys have no quota API, and Antigravity's cloudcode-pa quota calls 403 for this client.
const googleNote = "quota not exposed (AI Studio key; Antigravity quota endpoint denies access)"

// warnThreshold parses HARNESS_USAGE_WARN_PCT as float in (0,100], default 80; invalid → 80.
func warnThreshold() float64 {
	v := os.Getenv("HARNESS_USAGE_WARN_PCT")
	if v == "" {
		return 80
	}
	pct, err := strconv.ParseFloat(v, 64)
	if err != nil || pct <= 0 || pct > 100 {
		return 80
	}
	return pct
}

// setStates classifies each quota as "ok", "warn", or "exceeded" based on UsedPct and threshold.
func setStates(qs []quota, threshold float64) {
	for i := range qs {
		if qs[i].Note != "" && qs[i].Limit == 0 {
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
	var out []quota
	for _, m := range ghAccountRE.FindAllSubmatch(status, -1) {
		out = append(out, githubQuota(ctx, string(m[1])))
	}
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

// parseCopilot turns a /copilot_internal/user body into its premium-credit row.
func parseCopilot(login string, body []byte) quota {
	var u struct {
		Plan  string `json:"copilot_plan"`
		Reset string `json:"quota_reset_date"`
		Snaps struct {
			Premium struct {
				Entitlement float64 `json:"entitlement"`
				Used        float64 `json:"credits_used"`
				PctLeft     float64 `json:"percent_remaining"`
				Unlimited   bool    `json:"unlimited"`
			} `json:"premium_interactions"`
		} `json:"quota_snapshots"`
	}
	q := quota{Provider: "github", Account: login, Window: "monthly"}
	if err := json.Unmarshal(body, &u); err != nil {
		q.Note = "unavailable: unreadable response"
		return q
	}
	p := u.Snaps.Premium
	q.Plan, q.Resets = agent.CopilotPlanName(u.Plan), u.Reset
	if p.Unlimited {
		q.Note = "unlimited"
		return q
	}
	q.UsedPct, q.Used, q.Limit = math.Round((100-p.PctLeft)*10)/10, p.Used, p.Entitlement
	return q
}

// whamURL is ChatGPT's plan-usage endpoint codex itself polls; a var so tests can point it at httptest.
var whamURL = "https://chatgpt.com/backend-api/wham/usage"

// openaiQuotas reads codex's ChatGPT rate-limit windows (5h + weekly).
func openaiQuotas(ctx context.Context) []quota {
	tok, acct, keyringNote := agent.CodexAccessToken()
	if tok == "" {
		note := "unavailable: codex not signed in to ChatGPT"
		if keyringNote != "" {
			note += " (keyring: " + keyringNote + ")"
		}
		return []quota{{Provider: "openai", Note: note}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, whamURL, nil)
	if err != nil {
		return []quota{{Provider: "openai", Note: "unavailable: " + err.Error()}}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("ChatGPT-Account-Id", acct)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []quota{{Provider: "openai", Note: "unavailable: network error"}}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return []quota{{Provider: "openai", Note: fmt.Sprintf("unavailable: HTTP %d (run codex to refresh login)", resp.StatusCode)}}
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
		out = append(out, quota{Provider: "openai", Account: u.Email, Plan: title(u.Plan), Window: windowLabel(w.Seconds),
			UsedPct: w.UsedPct, Resets: time.Unix(w.ResetAt, 0).Local().Format("2006-01-02 15:04")})
	}
	if len(out) == 0 {
		out = append(out, quota{Provider: "openai", Account: u.Email, Plan: title(u.Plan), Note: "no rate-limit windows reported"})
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

func title(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
	fmt.Fprintf(out, "%-9s %-22s %-12s %-8s %-15s %8s  %-16s  %s\n", "PROVIDER", "ACCOUNT", "PLAN", "WINDOW", col, "STATE", "RESETS", "DETAIL")
	for _, q := range qs {
		pct, detail := q.UsedPct, q.Note
		if q.Group != "" {
			detail = q.Group
		}
		if show != "used" {
			pct = q.LeftPct
		}
		val, state := bar(pct)+" "+fmt.Sprintf("%4s", fmt.Sprintf("%.0f%%", pct)), q.State
		if q.Note != "" && q.Limit == 0 {
			val, state = "-", "-"
		}
		if q.Limit > 0 && q.Unit == "USD" {
			detail = fmt.Sprintf("$%.2f / $%.2f", q.Used, q.Limit)
			if show != "used" {
				detail = fmt.Sprintf("$%.2f of $%.2f left", max(q.Limit-q.Used, 0), q.Limit)
			}
		} else if q.Limit > 0 {
			if show == "used" {
				detail = fmt.Sprintf("%.0f / %.0f credits", q.Used, q.Limit)
			} else {
				detail = fmt.Sprintf("%.0f of %.0f credits left", q.Limit-q.Used, q.Limit)
			}
		}
		fmt.Fprintf(out, "%-9s %-22s %-12s %-8s %-15s %8s  %-16s  %s\n",
			q.Provider, dash(q.Account), dash(q.Plan), dash(q.Window), val, state, dash(q.Resets), detail)
	}
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
	fmt.Fprintf(out, "%-22s %-24s %10s %10s %10s\n", "PROVIDER", "MODEL", "TODAY", "30D", "ALL-TIME")
	for _, a := range aggs {
		fmt.Fprintf(out, "%-22s %-24s %10s %10s %10s\n",
			rowLabel(a.Provider, a.Profile), a.Model, humanTokens(a.Today), humanTokens(a.Last30d), humanTokens(a.AllTime))
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// agyUsageCmd is the agy exec seam; tests replace it with a fake binary so none of this reaches agy.
var agyUsageCmd = func(ctx context.Context) ([]byte, error) {
	bin, err := paths.VendorBin("agy")
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
		if _, lookErr := paths.VendorBin("agy"); lookErr != nil {
			return agyNote("agy not installed")
		}
		return agyNote("agy usage failed: " + firstLine(out, err))
	}
	if strings.Contains(string(out), "not logged in") || strings.Contains(string(out), "run agy login") {
		return agyNote("agy not logged in (run agy login)")
	}
	if i := strings.Index(string(out), "AGY_ERROR:"); i >= 0 {
		return agyNote("agy usage failed: " + firstLine(out[i:], err))
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
			out = append(out, quota{
				Provider: "google", Plan: "Antigravity", Account: "-",
				Window:   b.Window,
				UsedPct:  math.Round((1 - b.RemainingFrac) * 100),
				Resets:   formatAgyReset(b.ResetTime),
				ResetsAt: agyResetUTC(b.ResetTime),
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

// firstLine trims a command's combined error to one line, capped at 80 chars, never the whole stream.
func firstLine(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		s = err.Error()
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// formatAgyReset parses agy's RFC3339 reset_time and renders it local, matching the openai rows' format.
func formatAgyReset(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

// agyResetUTC parses agy's RFC3339 reset_time and normalizes it to a UTC instant for ResetsAt.
func agyResetUTC(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
