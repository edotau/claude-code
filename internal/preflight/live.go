package preflight

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// profileTimeout caps one profile's listing refresh.
const profileTimeout = 20 * time.Second

// Budget caps the install step's listing refresh; each pong then gets its own pongCap window; a var for tests.
var Budget = 45 * time.Second

// pongCap is how long after the step starts agent's install pong may run: opencode's cold start needs twice Budget.
func pongCap(agent string) time.Duration {
	if agent == "opencode" {
		return 2 * Budget
	}
	return Budget
}

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
			if err := fetch(lctx, prov, now); err != nil {
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

// line is the row, at most 80 columns.
func (p Pong) line() string {
	state := "ok"
	if !p.OK {
		state = "fail · " + cmp.Or(p.Err, "unknown")
	} else if p.Fallback != "" {
		state += " · " + p.Fallback
	}
	s := fmt.Sprintf("pong     %-8s · %s · %s · %s", p.Agent, cmp.Or(p.Profile, "-"), cmp.Or(p.Model, "default"), state)
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

// runAsk runs `<bin> agents ask --agent a [--profile p] --fallback none --format json`; a seam for tests.
var runAsk = defaultRunAsk

func defaultRunAsk(ctx context.Context, bin, agentName, profile string) (wire.Envelope, error) {
	argv := []string{"agents", "ask", "--agent", agentName}
	if profile != "" {
		argv = append(argv, "--profile", profile)
	}
	cmd := exec.CommandContext(ctx, bin, append(argv, "--fallback", "none", "--format", "json", "Reply with the single word: pong")...)
	proc.OwnGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	var env wire.Envelope
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
	return pongAll(ctx, bin, livePins(ps), "", nil)
}

// livePins keeps the pins a pong can reach: pinned, served and with a credential.
func livePins(ps []Pin) []Pin {
	var live []Pin
	for _, p := range ps {
		if p.Status != statusUnpinned && p.Served != "no" && !strings.HasPrefix(p.Detail, "no credential") {
			live = append(live, p)
		}
	}
	return live
}

// pongAll pongs every pin in parallel; askProfile is passed to ask as --profile; deadline, when set, caps each pong.
func pongAll(ctx context.Context, bin string, ps []Pin, askProfile string, deadline func(agent string) time.Time) []Pong {
	out := make([]Pong, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Go(func() {
			pctx := ctx
			if deadline != nil {
				var cancel context.CancelFunc
				pctx, cancel = context.WithDeadline(ctx, deadline(p.Agent))
				defer cancel()
			}
			out[i] = pong(pctx, bin, p, askProfile)
		})
	}
	wg.Wait()
	return out
}

func pong(ctx context.Context, bin string, p Pin, askProfile string) Pong {
	r := Pong{Agent: p.Agent, Profile: p.Profile, Model: p.Model}
	env, err := runAsk(ctx, bin, p.Agent, askProfile)
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

// RunInstallStep refreshes stale listings, then pongs each agent; silent only when no pin has a credential.
func RunInstallStep(ctx context.Context, w io.Writer, reg *providers.Registry, bin string, now time.Time) []Pong {
	ps := Check(reg, now)
	if slices.ContainsFunc(ps, func(p Pin) bool { return p.Status == StatusError }) {
		printFixRows(w, ps)
		return nil
	}
	if !anyCredential(reg, ps) {
		return nil // no credential anywhere: nothing to ping
	}
	rctx, rcancel := context.WithTimeout(ctx, Budget)
	printRefreshErrors(w, RefreshStale(rctx, reg, ps, now, false))
	rcancel()
	ps = Check(reg, now)
	pongStart := time.Now()                                      // pongs get their own window after the refresh, not what it left over
	ctx, cancel := context.WithTimeout(ctx, pongCap("opencode")) // the longest pong bounds the step
	defer cancel()
	pongs := pongAll(ctx, bin, livePins(ps), "", func(a string) time.Time { return pongStart.Add(pongCap(a)) })
	for _, r := range pongs {
		fmt.Fprintln(w, r.line())
	}
	printFixRows(w, ps)
	return pongs
}

// profileNames resolves profile to the profiles to pong, printing a skip row for each unusable one.
func profileNames(w io.Writer, reg *providers.Registry, profile string) []string {
	cands := []string{profile}
	if profile == "all" {
		cands = reg.ProfilesOf("databricks")
	}
	var names []string
	for _, n := range cands {
		prov := reg.Providers[n]
		switch {
		case prov == nil:
			fmt.Fprintf(w, "pong     skip · %s · unknown profile\n", n)
		case prov.Preset != providers.PresetDatabricks:
			fmt.Fprintf(w, "pong     skip · %s · not a Databricks profile\n", n)
		case !providers.CredentialPresent(prov):
			fmt.Fprintf(w, "pong     skip · %s · no credential\n", n)
		default:
			names = append(names, n)
		}
	}
	if len(names) == 0 { // a fail row makes doctor exit non-zero: no pong ran
		fmt.Fprintf(w, "pong     fail · %s · no usable Databricks profile\n", profile)
	}
	return names
}

// RunProfilePongs pongs every agent on profile (all = each usable Databricks profile), one row per agent x profile.
func RunProfilePongs(ctx context.Context, w io.Writer, reg *providers.Registry, bin, profile string) []Pong {
	var all []Pong
	for _, n := range profileNames(w, reg, profile) {
		pctx, cancel := context.WithTimeout(ctx, 2*Budget) // five cold agents in parallel outlast one install-step budget
		var ps []Pin
		for _, a := range slices.Sorted(slices.Values(router.TableAgents)) {
			ps = append(ps, Pin{Agent: a, Profile: n})
		}
		for _, r := range pongAll(pctx, bin, ps, n, nil) {
			fmt.Fprintln(w, r.line())
			all = append(all, r)
		}
		cancel()
	}
	return all
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

// RefreshPinned force re-lists every pinned Databricks profile's endpoints; returns the load or listing errors.
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
		fmt.Fprintf(w, "pins     %s %s: %s\n", p.Agent, p.Status, cmp.Or(p.Fix, p.Detail))
	}
}

// notOK returns the pins whose status is not OK.
func notOK(ps []Pin) []Pin {
	return slices.DeleteFunc(slices.Clone(ps), func(p Pin) bool { return p.Status == StatusOK })
}

func anyCredential(reg *providers.Registry, ps []Pin) bool {
	return slices.ContainsFunc(ps, func(p Pin) bool {
		prov := reg.Providers[p.Profile]
		return prov != nil && providers.CredentialPresent(prov)
	})
}

// StaticLine is the session-start line for a down proxy or a bad pin, from cached listings only; "" when all is well.
func StaticLine(reg *providers.Registry, now time.Time) string {
	if reg == nil {
		return ""
	}
	ps := Check(reg, now)
	if len(ps) == 1 && ps[0].Agent == "vault" { // Check's unreadable-vault diagnostic
		return "Preflight: " + fieldsLine(ps[0].Detail)
	}
	dbx := profiles(reg, ps)
	var session string // claude's row is present exactly when the session has a profile
	if i := slices.IndexFunc(ps, func(p Pin) bool { return p.Agent == "claude" }); i >= 0 {
		session = ps[i].Profile
	}
	if prov := reg.Providers[session]; len(dbx) == 0 && (prov == nil || prov.Preset != providers.PresetDatabricks) {
		return "" // no Databricks profile in play
	}
	var parts []string
	for _, p := range notOK(ps) {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", p.Agent, p.Status, cmp.Or(p.Detail, p.Profile)))
	}
	if len(dbx) > 0 && !proxyUp() {
		parts = append([]string{"proxy not running"}, parts...)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Preflight: " + strings.Join(parts, "; ") + " (claude-code doctor)"
}

// proxyUp reads the proxy state file and checks its pid is alive.
func proxyUp() bool {
	st, err := gateway.ReadState()
	return err == nil && st.PID > 0 && proc.Alive(st.PID)
}

// Claim takes the detached served-refresh slot; false when another attempt started within refreshRetry.
func Claim(now time.Time) bool { return catalog.ClaimNamed("refresh-attempt", now) }

// DiscoverAfterAuth refreshes every discover target's model list in the background after a sign-in.
func DiscoverAfterAuth(reg *providers.Registry) {
	for _, t := range catalog.DiscoverTargets(reg) {
		catalog.SpawnDiscover(t, true)
	}
}

// refresh is the serving rows' live listing into the vault snapshot and the router's copy; a seam for tests.
var refresh = catalog.Refresh

// fetch lists p's endpoints live and caches them: serving rows in the vault snapshot and the router's copy.
func fetch(ctx context.Context, p *providers.Provider, now time.Time) error {
	if !p.IsAIGateway() {
		return refresh(ctx, p)
	}
	infos, err := providers.ListModels(ctx, p)
	if err != nil {
		return err
	}
	return router.WarmServed(p.Name, infos, now)
}
