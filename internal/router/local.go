package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	defaultProbeTimeout = 300 * time.Millisecond
	defaultProbeTTL     = 10 * time.Second
	defaultAdmitPoll    = 250 * time.Millisecond
)

// Probe and admission timing; vars so tests shorten them.
var (
	localProbeTimeout = defaultProbeTimeout
	localProbeTTL     = defaultProbeTTL
	localAdmitPoll    = defaultAdmitPoll
	localNow          = time.Now
)

// LocalDown is why the last-resort row is not usable.
type LocalDown string

// The typed reasons a probe fails.
const (
	LocalRefused     LocalDown = "connection refused"
	LocalTimeout     LocalDown = "timeout"
	LocalBadStatus   LocalDown = "bad status"
	LocalNoModel     LocalDown = "no model loaded"
	LocalBadBody     LocalDown = "bad response"
	LocalUnreachable LocalDown = "unreachable"
	LocalNoRow       LocalDown = "no last-resort row"
	LocalCancelled   LocalDown = "cancelled"
)

// LocalProbe is one probe's outcome: Down is empty when up.
type LocalProbe struct {
	Down   LocalDown
	Detail string
	Model  string
	At     time.Time
	Took   time.Duration
}

// Up reports whether the row answered with its model listed.
func (p LocalProbe) Up() bool { return p.Down == "" }

type probeEntry struct {
	key string
	res LocalProbe
}

var probeCache struct {
	sync.Mutex
	e probeEntry
}

// LocalUp probes the last-resort row (up: true and its model id; down: false and the typed reason), cached 10 s.
func LocalUp(ctx context.Context, reg *providers.Registry) (bool, string) {
	p := ProbeLocal(ctx, reg)
	if p.Up() {
		return true, p.Model
	}
	if p.Detail != "" {
		return false, string(p.Down) + " (" + p.Detail + ")"
	}
	return false, string(p.Down)
}

// ProbeLocal is LocalUp with the typed result; a failure replaces any cached success at once.
func ProbeLocal(ctx context.Context, reg *providers.Registry) LocalProbe {
	row := providers.LastResort(reg)
	if row == nil {
		return LocalProbe{Down: LocalNoRow, At: localNow()}
	}
	key := row.Name + "|" + probeURL(row) + "|" + row.Models["sonnet"]
	probeCache.Lock()
	defer probeCache.Unlock()
	if c := probeCache.e; c.key == key && localNow().Sub(c.res.At) < localProbeTTL {
		return c.res
	}
	res := probeOnce(ctx, row)
	if res.Down != LocalCancelled {
		probeCache.e = probeEntry{key: key, res: res}
	}
	return res
}

func probeOnce(parent context.Context, row *providers.Provider) LocalProbe {
	start := localNow()
	res := LocalProbe{At: start}
	ctx, cancel := context.WithTimeout(parent, localProbeTimeout)
	defer cancel()
	res.Down, res.Detail, res.Model = fetchModels(ctx, parent, row)
	res.Took = localNow().Sub(start)
	return res
}

// probeURL is the models URL on the host traffic uses (routes.anthropic), with base_url's path when set.
func probeURL(row *providers.Provider) string {
	base := strings.TrimSuffix(row.BaseURL, "/")
	route, err := url.Parse(row.Routes["anthropic"])
	if err != nil || route.Scheme == "" || route.Host == "" {
		return base + "/models"
	}
	origin := route.Scheme + "://" + route.Host
	if base == "" {
		return origin + strings.TrimSuffix(route.EscapedPath(), "/") + "/v1/models"
	}
	b, err := url.Parse(base)
	if err != nil {
		return base + "/models"
	}
	return origin + strings.TrimSuffix(b.EscapedPath(), "/") + "/models"
}

func fetchModels(ctx, parent context.Context, row *providers.Provider) (LocalDown, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL(row), nil)
	if err != nil {
		return LocalBadBody, err.Error(), ""
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return classifyProbeErr(err, parent), "", ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return LocalBadStatus, strconv.Itoa(resp.StatusCode), ""
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&body); err != nil {
		if ctx.Err() != nil {
			return classifyProbeErr(ctx.Err(), parent), "", ""
		}
		return LocalBadBody, "", ""
	}
	want := row.Models["sonnet"]
	for _, m := range body.Data {
		if m.ID == want {
			return "", "", m.ID
		}
	}
	if len(body.Data) == 0 {
		return LocalNoModel, "empty list", ""
	}
	return LocalNoModel, want + " not listed", ""
}

func classifyProbeErr(err error, parent context.Context) LocalDown {
	var ne net.Error
	switch {
	case parent.Err() != nil:
		return LocalCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return LocalTimeout
	case strings.Contains(err.Error(), "refused"):
		return LocalRefused
	}
	return LocalUnreachable
}

// LocalOutcome is how an admission ended.
type LocalOutcome string

// The typed admission outcomes; Admitted is the nil-error case.
const (
	LocalAdmitted LocalOutcome = "admitted"
	LocalBusy     LocalOutcome = "busy"
	LocalCanceled LocalOutcome = "cancelled"
)

// LocalAdmitError is a refused admission: Busy (Running holders, Waited long) or Cancelled (wraps the context error).
type LocalAdmitError struct {
	Outcome LocalOutcome
	Running int
	Waited  time.Duration
	Err     error
}

func (e *LocalAdmitError) Error() string {
	if e.Outcome == LocalCanceled {
		return fmt.Sprintf("local admission cancelled after %s: %v", e.Waited.Round(time.Millisecond), e.Err)
	}
	return fmt.Sprintf("local model busy (%d running); waited %s", e.Running, e.Waited.Round(time.Millisecond))
}

func (e *LocalAdmitError) Unwrap() error { return e.Err }

var localWaiting atomic.Int64

// LocalAdmit takes the one machine-wide local slot, polling up to wait; release is never nil.
func LocalAdmit(ctx context.Context, wait time.Duration) (release func(), err error) {
	reg, lerr := providers.Load()
	var row *providers.Provider
	if lerr == nil {
		row = providers.LastResort(reg)
	}
	if row == nil {
		return func() {}, nil // no row to protect: fail open like claimLimitedRecord
	}
	return admitLocal(ctx, wait, AccountOf(row), CurrentLimits())
}

func admitLocal(ctx context.Context, wait time.Duration, account string, lim inflight.Limits) (func(), error) {
	noop := func() {}
	start := localNow()
	claim := inflight.Claim{Owner: os.Getpid(), ID: "local." + strconv.FormatInt(start.UnixNano(), 10), Account: account}
	waiting := false
	defer func() {
		if waiting {
			localWaiting.Add(-1)
		}
	}()
	for {
		ok, snap := inflight.TryClaim(claim, lim)
		if ok {
			var once sync.Once
			return func() { once.Do(func() { inflight.Release(claim.Owner, claim.ID) }) }, nil
		}
		left := wait - localNow().Sub(start)
		if left <= 0 {
			return noop, &LocalAdmitError{Outcome: LocalBusy, Running: snap.Per[account], Waited: localNow().Sub(start)}
		}
		if !waiting {
			waiting = true
			localWaiting.Add(1)
		}
		t := time.NewTimer(min(left, localAdmitPoll))
		select {
		case <-ctx.Done():
			t.Stop()
			return noop, &LocalAdmitError{Outcome: LocalCanceled, Running: snap.Per[account],
				Waited: localNow().Sub(start), Err: ctx.Err()}
		case <-t.C:
		}
	}
}

// localNote is state/harness/router/local.json: the last fallback to the local row.
type localNote struct {
	Schema  int       `json:"schema"`
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Surface string    `json:"surface"`
	Profile string    `json:"profile,omitempty"`
}

const localNoteSchema = 1

var _ = schemas.Register(schemas.Format{
	Name: "route-local", Owner: "internal/router", Kind: schemas.Cache, Version: localNoteSchema,
	Glob: localNoteFile,
	New:  func() any { return new(localNote) },
	Keep: "rebuildable cache: only the last-fallback hint for doctor and the statusline",
})

func localNoteFile() string { return filepath.Join(paths.StateDir(), "router", "local.json") }

// NoteLocalFallback records that kind sent surface on profile to the local row; "" profile marks every session.
func NoteLocalFallback(profile, kind, surface string) {
	n := localNote{Schema: localNoteSchema, Time: localNow().UTC(), Kind: kind, Surface: surface, Profile: profile}
	writeLocalNote(n)
	audit.Log(audit.RouterFallback, "local", "kind="+kind+" surface="+surface+" profile="+profile)
}

func writeLocalNote(n localNote) {
	b, err := json.Marshal(n)
	if err != nil {
		return
	}
	_ = paths.AtomicWriteNoSync(localNoteFile(), b, 0o600) // a hint: a lost write costs one doctor field
}

// LastLocalFallback reads the note only; an empty profile reads any note, ok is false on none or another profile.
func LastLocalFallback(profile string) (n time.Time, kind, surface string, ok bool) {
	b, err := os.ReadFile(localNoteFile())
	var v localNote
	if err != nil || json.Unmarshal(b, &v) != nil || v.Schema != localNoteSchema ||
		profile != "" && v.Profile != "" && v.Profile != profile {
		return time.Time{}, "", "", false
	}
	return v.Time, v.Kind, v.Surface, true
}

// LocalState is what a doctor row prints.
type LocalState struct {
	Up       bool
	Reason   string // why it is down; empty when up
	Model    string
	Running  int // slots held machine-wide
	Waiting  int // callers of this process waiting for the slot
	LastAt   time.Time
	LastKind string
}

// LocalStatus probes (cached) and counts holders; it reads the registry, so it is for doctor, not a hot path.
func LocalStatus() LocalState {
	var st LocalState
	reg, err := providers.Load()
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	st.Up, st.Reason = LocalUp(context.Background(), reg)
	if st.Up {
		st.Model, st.Reason = st.Reason, ""
	}
	if row := providers.LastResort(reg); row != nil {
		st.Running = inflight.Count().Per[AccountOf(row)]
	}
	st.Waiting = int(localWaiting.Load())
	st.LastAt, st.LastKind, _, _ = LastLocalFallback("")
	return st
}
