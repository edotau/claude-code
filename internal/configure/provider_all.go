// provider_all.go — `configure provider --all`: check every provider's sign-in, log in where needed, recheck.
package configure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// Seams for --all; tests replace the sign-in factory, its cap and the TTY gates.
var (
	allSigninFor    = agent.SigninFor
	allCheckTimeout = 12 * time.Second
	allStdinTTY     = isStdinTTY
	allStderrTTY    = func() bool { return ansi.IsTerminal(os.Stderr) }
)

// Row states reported by --all.
const (
	allOK         = "ok"
	allNeedsLogin = "needs login"
	allRejected   = "rejected"
	allTimedOut   = "timed out"
	allError      = "error"
)

// AllRowStatus is one row's verdict; Detail is the expiry text or error, never a token.
type AllRowStatus struct {
	Row    string `json:"row"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// allRows lists the provider names (only profile when set), sorted.
func allRows(reg *providers.Registry, profile string) []string {
	var rows []string
	if reg == nil {
		return rows
	}
	for n, p := range reg.Providers {
		if p != nil && (profile == "" || n == profile) {
			rows = append(rows, n)
		}
	}
	slices.Sort(rows)
	return rows
}

// namedProvider returns p with its registry key filled in as Name.
func namedProvider(name string, p *providers.Provider) *providers.Provider {
	if p.Name != "" {
		return p
	}
	cp := *p
	cp.Name = name
	return &cp
}

// allCheckOne runs the sign-in check capped at allCheckTimeout; a timeout is transient, never "needs login".
func allCheckOne(ctx context.Context, p *providers.Provider) AllRowStatus {
	st := AllRowStatus{Row: p.Name}
	type res struct {
		v      providers.AuthVerdict
		detail string
	}
	done := make(chan res, 1)
	si := allSigninFor(p) // resolved before the goroutine: a timed-out check must not read the seam later
	go func() { v, d := si.Check(ctx); done <- res{v, d} }()
	select {
	case r := <-done:
		st.Detail = r.detail
		switch r.v {
		case providers.VerdictOK:
			st.Status = allOK
		case providers.VerdictNeedsLogin:
			st.Status = allNeedsLogin
		case providers.VerdictRejected:
			st.Status = allRejected
		default:
			st.Status = allError
		}
	case <-time.After(allCheckTimeout):
		st.Status = allTimedOut
	}
	return st
}

// allCheckAll checks the rows concurrently and returns the verdicts in row order.
func allCheckAll(ctx context.Context, reg *providers.Registry, rows []string) []AllRowStatus {
	out := make([]AllRowStatus, len(rows))
	var wg sync.WaitGroup
	for i, n := range rows {
		wg.Go(func() {
			out[i] = allCheckOne(ctx, namedProvider(n, reg.Providers[n]))
		})
	}
	wg.Wait()
	return out
}

func allLine(s AllRowStatus) string {
	switch {
	case s.Status == allError:
		return fmt.Sprintf("%s: error: %s", s.Row, s.Detail)
	case s.Detail != "":
		return fmt.Sprintf("%s: %s (%s)", s.Row, s.Status, s.Detail)
	}
	return fmt.Sprintf("%s: %s", s.Row, s.Status)
}

// runProviderAll checks every row, offers login to failing rows on a TTY (not in JSON mode); true when all ok.
func runProviderAll(d *dialog) bool {
	ctx := context.Background()
	rows := allRows(d.reg, d.o.Profile)
	if len(rows) == 0 {
		if d.o.JSON {
			fmt.Fprintln(d.out, "[]")
		} else {
			d.println("no providers configured")
		}
		return true
	}
	res := allCheckAll(ctx, d.reg, rows)
	if !d.o.JSON {
		for _, s := range res {
			d.println(allLine(s))
		}
	}
	if !d.o.JSON && !d.o.Check && !d.o.DryRun && allStdinTTY() && allStderrTTY() {
		for i, s := range res {
			if s.Status != allNeedsLogin && s.Status != allRejected {
				continue
			}
			if res[i] = allLoginRow(d, s); res[i].Status != s.Status || res[i].Detail != s.Detail {
				d.println(allLine(res[i]))
			}
		}
	}
	allGood := true
	for _, s := range res {
		allGood = allGood && s.Status == allOK
	}
	if d.o.JSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(d.out, string(b))
	}
	return allGood
}

// allLoginRow prompts, logs in and re-checks one failing row; the returned status is the final verdict.
func allLoginRow(d *dialog, s AllRowStatus) AllRowStatus {
	fmt.Fprintf(os.Stderr, "%s: login required. Log in now? [Y/n] ", s.Row)
	if !yes(d.br) {
		return s
	}
	ctx := context.Background()
	p := namedProvider(s.Row, d.reg.Providers[s.Row])
	if err := allSigninFor(p).Login(ctx, d.o.In, os.Stderr); err != nil {
		return AllRowStatus{Row: s.Row, Status: allError, Detail: "login: " + err.Error()}
	}
	d.changed = true
	d.reload()
	cur := d.reg.Providers[s.Row]
	if cur == nil {
		return s
	}
	return allCheckOne(ctx, namedProvider(s.Row, cur))
}
