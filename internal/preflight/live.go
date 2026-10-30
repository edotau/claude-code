package preflight

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// profileTimeout caps one profile's listing refresh.
const profileTimeout = 20 * time.Second

// Budget is the wall-clock cap on the whole install step (refresh and pongs); a var for tests.
var Budget = 45 * time.Second

// RefreshStale re-lists every Databricks profile in use whose cache is missing or past TTL (all when force); errors skip.
func RefreshStale(ctx context.Context, reg *providers.Registry, ps []Pin, now time.Time, force bool) []error {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)
	for _, prov := range Profiles(reg, ps) {
		if c, ok := LoadServed(prov.Name); ok && !force && c.Fresh(now) {
			continue
		}
		wg.Go(func() {
			lctx, cancel := context.WithTimeout(ctx, profileTimeout)
			defer cancel()
			if _, err := Refresh(lctx, reg, prov, now); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", prov.Name, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errs
}

// Pong is one agent's live round trip on its pinned profile.
type Pong struct {
	Agent, Profile, Model, Fallback string
	OK                              bool
	Err                             string
}

// Line is the row, at most 80 columns.
func (p Pong) Line() string {
	state, fb := "ok", cmp.Or(p.Fallback, "none")
	if !p.OK {
		state, fb = "fail", cmp.Or(p.Err, "unknown")
	}
	s := fmt.Sprintf("pong %-8s · %s · %s · %s · %s", p.Agent, cmp.Or(p.Profile, "-"), cmp.Or(p.Model, "default"), state, fb)
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

// envelope is the part of `ask --format json` read here (agent.Envelope imports this package's callers).
type envelope struct {
	Status   string  `json:"status"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Fallback *string `json:"fallback"`
	Error    *struct {
		Kind string `json:"kind"`
	} `json:"error"`
}

// runAsk runs `<bin> ask --agent a --fallback none --format json` and returns the envelope; a seam for tests.
var runAsk = defaultRunAsk

func defaultRunAsk(ctx context.Context, bin, agentName string) (envelope, error) {
	cmd := exec.CommandContext(ctx, bin, "ask", "--agent", agentName, "--fallback", "none", "--format", "json",
		"Reply with the single word: pong")
	groupKill(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	var env envelope
	if jerr := json.Unmarshal(out, &env); jerr != nil {
		if err != nil {
			return env, err
		}
		return env, jerr
	}
	return env, nil
}

// LivePongs asks every pinned agent (profile resolved, credential present) for "pong" in parallel within ctx's deadline.
func LivePongs(ctx context.Context, bin string, ps []Pin) []Pong {
	var live []Pin
	for _, p := range ps {
		if p.Status != StatusUnpinned && p.Served != "no" && !strings.HasPrefix(p.Detail, "no credential") {
			live = append(live, p)
		}
	}
	out := make([]Pong, len(live))
	var wg sync.WaitGroup
	for i, p := range live {
		wg.Go(func() {
			out[i] = pong(ctx, bin, p)
		})
	}
	wg.Wait()
	return out
}

func pong(ctx context.Context, bin string, p Pin) Pong {
	r := Pong{Agent: p.Agent, Profile: p.Profile, Model: p.Model}
	env, err := runAsk(ctx, bin, p.Agent)
	r.Profile, r.Model = cmp.Or(env.Provider, r.Profile), cmp.Or(env.Model, r.Model)
	switch {
	case err != nil:
		r.Err = oneLine(err.Error())
		if ctx.Err() != nil {
			r.Err = "timeout"
		}
	case env.Status != "ok":
		r.Err = "status " + env.Status
		if env.Error != nil {
			r.Err = env.Error.Kind
		}
	default:
		r.OK = true
		if env.Fallback != nil {
			r.Fallback = *env.Fallback
		}
	}
	return r
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// RunInstallStep is the post-install preflight: refresh stale listings, then print one live pong row per agent.
// It prints nothing when no agent has a credential, and never returns an error.
func RunInstallStep(ctx context.Context, w io.Writer, reg *providers.Registry, bin string, now time.Time) {
	ps := Check(reg, now)
	if len(Profiles(reg, ps)) == 0 && !anyCredential(reg, ps) {
		return // no credential anywhere: nothing to ping
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	for _, err := range RefreshStale(ctx, reg, ps, now, false) {
		fmt.Fprintf(w, "preflight  listing refresh failed: %s\n", oneLine(err.Error()))
	}
	ps = Check(reg, now)
	for _, r := range LivePongs(ctx, bin, ps) {
		fmt.Fprintln(w, r.Line())
	}
	printFixRows(w, ps)
}

// Stale reports whether a Databricks profile in use has no fresh listing.
func Stale(reg *providers.Registry, now time.Time) bool {
	return slices.ContainsFunc(Profiles(reg, Check(reg, now)), func(p *providers.Provider) bool {
		c, ok := LoadServed(p.Name)
		return !ok || !c.Fresh(now)
	})
}

// Warm re-lists stale profiles and prints each pin needing a fix when served routing is on; never fails the caller.
func Warm(w io.Writer, reg *providers.Registry, now time.Time) {
	if !providers.ServedRoutingOn() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
	defer cancel()
	for _, err := range RefreshStale(ctx, reg, Check(reg, now), now, false) {
		fmt.Fprintf(w, "preflight  listing refresh failed: %s\n", oneLine(err.Error()))
	}
	printFixRows(w, Check(reg, now))
}

// printFixRows prints one line per pin that is not OK, with its fix or detail.
func printFixRows(w io.Writer, ps []Pin) {
	for _, p := range ps {
		if p.Status != StatusOK {
			fmt.Fprintf(w, "%s %s: %s\n", p.Agent, p.Status, cmp.Or(p.Fix, p.Detail))
		}
	}
}

func anyCredential(reg *providers.Registry, ps []Pin) bool {
	for _, p := range ps {
		if prov := reg.Providers[p.Profile]; prov != nil && providers.CredentialPresent(prov) {
			return true
		}
	}
	return false
}

// StaticLine is the session-start context line: proxy down, or any pin that is unpinned, stale, unserved or
// credential-less, from the cached listings only (no network, no model call). "" when all is well.
func StaticLine(reg *providers.Registry, now time.Time) string {
	if reg == nil {
		return ""
	}
	ps := Check(reg, now)
	if prov := reg.Providers[providers.AgentPins()["claude"].Profile]; len(Profiles(reg, ps)) == 0 &&
		(prov == nil || prov.Preset != providers.PresetDatabricks) {
		return "" // no Databricks profile in play
	}
	var parts []string
	for _, p := range ps {
		if p.Status != StatusOK {
			parts = append(parts, fmt.Sprintf("%s %s (%s)", p.Agent, p.Status, cmp.Or(p.Detail, p.Profile)))
		}
	}
	if len(Profiles(reg, ps)) > 0 && !proxyUp() {
		parts = append([]string{"proxy not running"}, parts...)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Preflight: " + strings.Join(parts, "; ") + " (claude-code doctor)"
}

// proxyUp reads router.json and checks its pid is alive.
func proxyUp() bool {
	b, err := os.ReadFile(paths.StateDir() + "/router.json")
	if err != nil {
		return false
	}
	var st struct {
		PID int `json:"pid"`
	}
	return json.Unmarshal(b, &st) == nil && st.PID > 0 && proc.Alive(st.PID)
}
