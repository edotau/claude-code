package preflight

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// profileTimeout caps one profile's listing refresh.
const profileTimeout = 20 * time.Second

// Budget is the wall-clock cap on the whole install step (refresh and pongs); a var for tests.
var Budget = 45 * time.Second

// RefreshStale re-lists Databricks profiles in use with a missing or stale cache (all when force); errors skip.
func RefreshStale(ctx context.Context, reg *providers.Registry, ps []Pin, now time.Time, force bool) []error {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)
	for _, prov := range profiles(reg, ps) {
		if !force && listingFresh(prov.Name, now) {
			continue
		}
		wg.Go(func() {
			lctx, cancel := context.WithTimeout(ctx, profileTimeout)
			defer cancel()
			if _, err := fetch(lctx, reg, prov, now); err != nil {
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
	proc.OwnGroup(cmd)
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

// LivePongs asks every pinned agent (profile resolved, credential present) for "pong" in parallel within ctx.
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
		r.Err = fieldsLine(err.Error())
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

// listingFresh reports whether name has a cached listing within its TTL.
func listingFresh(name string, now time.Time) bool {
	c, ok := router.LoadServed(name)
	return ok && c.Fresh(now)
}

// printRefreshErrors prints one line per failed listing refresh.
func printRefreshErrors(w io.Writer, errs []error) {
	for _, err := range errs {
		fmt.Fprintf(w, "preflight  listing refresh failed: %s\n", fieldsLine(err.Error()))
	}
}

func fieldsLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// RunInstallStep refreshes stale listings, then prints one live pong row per agent; silent without a credential.
func RunInstallStep(ctx context.Context, w io.Writer, reg *providers.Registry, bin string, now time.Time) {
	ps := Check(reg, now)
	if !anyCredential(reg, ps) {
		return // no credential anywhere: nothing to ping
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	printRefreshErrors(w, RefreshStale(ctx, reg, ps, now, false))
	ps = Check(reg, now)
	for _, r := range LivePongs(ctx, bin, ps) {
		fmt.Fprintln(w, r.Line())
	}
	printFixRows(w, ps)
}

// Stale reports whether a Databricks profile in use has no fresh listing.
func Stale(reg *providers.Registry, now time.Time) bool {
	return slices.ContainsFunc(profiles(reg, Check(reg, now)), func(p *providers.Provider) bool {
		return !listingFresh(p.Name, now)
	})
}

// Warm re-lists stale profiles and prints each pin needing a fix when served routing is on; never fails the caller.
func Warm(w io.Writer, reg *providers.Registry, now time.Time) {
	if !providers.ServedRoutingOn() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
	defer cancel()
	printRefreshErrors(w, RefreshStale(ctx, reg, Check(reg, now), now, false))
	printFixRows(w, Check(reg, now))
}

// RefreshPinned force re-lists every pinned Databricks profile's endpoints; a registry load failure is the only error.
func RefreshPinned(ctx context.Context) []error {
	reg, err := providers.Load()
	if err != nil {
		return []error{err}
	}
	now := time.Now()
	return RefreshStale(ctx, reg, Check(reg, now), now, true)
}

// printFixRows prints one line per pin that is not OK, with its fix or detail.
func printFixRows(w io.Writer, ps []Pin) {
	for _, p := range notOK(ps) {
		fmt.Fprintf(w, "%s %s: %s\n", p.Agent, p.Status, cmp.Or(p.Fix, p.Detail))
	}
}

// notOK returns the pins whose status is not OK.
func notOK(ps []Pin) []Pin {
	return slices.DeleteFunc(slices.Clone(ps), func(p Pin) bool { return p.Status == StatusOK })
}

func anyCredential(reg *providers.Registry, ps []Pin) bool {
	for _, p := range ps {
		if prov := reg.Providers[p.Profile]; prov != nil && providers.CredentialPresent(prov) {
			return true
		}
	}
	return false
}

// StaticLine is the session-start line for a down proxy or a bad pin, from cached listings only; "" when all is well.
func StaticLine(reg *providers.Registry, now time.Time) string {
	if reg == nil {
		return ""
	}
	ps := Check(reg, now)
	if prov := reg.Providers[agent.AgentPins()["claude"].Profile]; len(profiles(reg, ps)) == 0 &&
		(prov == nil || prov.Preset != providers.PresetDatabricks) {
		return "" // no Databricks profile in play
	}
	var parts []string
	for _, p := range notOK(ps) {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", p.Agent, p.Status, cmp.Or(p.Detail, p.Profile)))
	}
	if len(profiles(reg, ps)) > 0 && !proxyUp() {
		parts = append([]string{"proxy not running"}, parts...)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Preflight: " + strings.Join(parts, "; ") + " (claude-code doctor)"
}

// proxyUp reads the proxy state file and checks its pid is alive.
func proxyUp() bool {
	b, err := os.ReadFile(paths.StateDir() + "/proxy.json")
	if errors.Is(err, os.ErrNotExist) {
		b, err = os.ReadFile(paths.StateDir() + "/router.json") // a daemon from an older build; drop after 2026-11-14
	}
	if err != nil {
		return false
	}
	var st struct {
		PID int `json:"pid"`
	}
	return json.Unmarshal(b, &st) == nil && st.PID > 0 && proc.Alive(st.PID)
}

// refreshRetry spaces detached refresh attempts, so an unreachable workspace doesn't spawn one per SessionStart.
const refreshRetry = 15 * time.Minute

// Claim takes the detached served-refresh slot; false when another attempt started within refreshRetry.
func Claim(now time.Time) bool { return ClaimNamed("refresh-attempt", now) }

// ClaimNamed takes the detached-refresh slot stamped name; false when another attempt started within refreshRetry.
func ClaimNamed(name string, now time.Time) bool {
	path := filepath.Join(paths.StateDir(), "preflight", name)
	if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) < refreshRetry {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = os.WriteFile(path, nil, 0o600) // best effort: an unwritable stamp must not stop the refresh
	}
	return true
}

// listModels is the live workspace listing; a seam for tests.
var listModels = providers.ListModels

// fetch lists p's endpoints live and caches them.
func fetch(ctx context.Context, reg *providers.Registry, p *providers.Provider, now time.Time) (router.Served, error) {
	infos, err := listModels(ctx, reg, p)
	if err != nil {
		return router.Served{}, err
	}
	s := router.Served{Profile: p.Name, FetchedAt: now.UTC()}
	for _, m := range infos {
		s.Models = append(s.Models, router.ServedModel{ID: m.ID, Task: m.Task, Ready: m.Ready})
	}
	return s, router.SaveServed(s)
}
