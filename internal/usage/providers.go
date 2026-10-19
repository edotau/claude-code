// providers.go — live, account-wide plan quota per provider: GitHub Copilot (every gh login), OpenAI (codex's
// ChatGPT sign-in), Google (no quota API; a fixed row). Tokens stay in memory and child env, never printed.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/vendors"
)

// quota is one provider-account-window row; Note replaces the numbers when the lookup failed or has none.
type quota struct {
	Provider string  `json:"provider"`
	Account  string  `json:"account,omitempty"`
	Plan     string  `json:"plan,omitempty"`
	Window   string  `json:"window,omitempty"`
	UsedPct  float64 `json:"used_percent"`
	Used     float64 `json:"used_credits,omitempty"`
	Limit    float64 `json:"limit_credits,omitempty"`
	Resets   string  `json:"resets,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// googleNote: AI Studio keys have no quota API, and Antigravity's cloudcode-pa quota calls 403 for this client.
const googleNote = "quota not exposed (AI Studio key; Antigravity quota endpoint denies access)"

// fetchQuotas is the network seam; tests replace it so no test reaches gh or chatgpt.com.
var fetchQuotas = func() []quota {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var gh, oa []quota
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); gh = githubQuotas(ctx) }()
	go func() { defer wg.Done(); oa = openaiQuotas(ctx) }()
	wg.Wait()
	return append(append(gh, oa...), quota{Provider: "google", Note: googleNote})
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
		login := string(m[1])
		tok, err := ghRun(ctx, nil, "auth", "token", "--hostname", "github.com", "--user", login)
		if err != nil {
			out = append(out, quota{Provider: "github", Account: login, Note: "unavailable: no token"})
			continue
		}
		body, err := ghRun(ctx, []string{"GH_TOKEN=" + strings.TrimSpace(string(tok))}, "api", "/copilot_internal/user")
		if err != nil {
			out = append(out, quota{Provider: "github", Account: login, Note: "unavailable: no Copilot seat or API error"})
			continue
		}
		out = append(out, parseCopilot(login, body))
	}
	if len(out) == 0 {
		out = append(out, quota{Provider: "github", Note: "unavailable: no gh login"})
	}
	return out
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
	q.Plan, q.Resets = vendors.CopilotPlanName(u.Plan), u.Reset
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
	tok, acct := vendors.CodexAccessToken()
	if tok == "" {
		return []quota{{Provider: "openai", Note: "unavailable: codex not signed in to ChatGPT"}}
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

// printQuotas renders the provider rows below the transcript table.
func printQuotas(out io.Writer, qs []quota) {
	if len(qs) == 0 {
		return
	}
	fmt.Fprintf(out, "\nPROVIDER QUOTAS (live, account-wide)\n")
	fmt.Fprintf(out, "%-9s %-22s %-9s %-8s %6s  %-16s  %s\n", "PROVIDER", "ACCOUNT", "PLAN", "WINDOW", "USED", "RESETS", "DETAIL")
	for _, q := range qs {
		used, detail := fmt.Sprintf("%.0f%%", q.UsedPct), q.Note
		if q.Note != "" && q.Limit == 0 {
			used = "-"
		}
		if q.Limit > 0 {
			detail = fmt.Sprintf("%.0f / %.0f credits", q.Used, q.Limit)
		}
		fmt.Fprintf(out, "%-9s %-22s %-9s %-8s %6s  %-16s  %s\n",
			q.Provider, dash(q.Account), dash(q.Plan), dash(q.Window), used, dash(q.Resets), detail)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
