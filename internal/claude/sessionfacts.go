package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
)

// legacyLaunchFacts reads pre-5.1 session-launch files where fence was stored as a JSON string.
type legacyLaunchFacts struct {
	Profile string `json:"profile"`
	Model   string `json:"model,omitempty"`
	Fence   string `json:"fence,omitempty"`
}

// sessionRecord is the launch overlay saved per session id, plus any legacy fence doc from pre-5.1 files.
type sessionRecord struct {
	ov       agentconfig.Overlay
	fenceDoc string
}

var sessionIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// noFreshID are the flags after which the launcher cannot name the session id: a resume, a headless run, a named id.
var noFreshID = []string{"-p", "--print", "-r", "--resume", "-c", "--continue", "--session-id", "--from-pr"}

func factsPath(id string) string {
	return filepath.Join(paths.StateDir(), "session-launch", id+".json")
}

// flagArg is the uuid value of the last name flag in args before "--", as claude reads it ("" when none or no uuid).
func flagArg(args []string, names ...string) string {
	got := ""
	for i, a := range args {
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(a, "=")
		if short := shortAttached(a, names); short != "" {
			name, val, hasVal = short, a[len(short):], true
		}
		if !slices.Contains(names, name) {
			continue
		}
		if !hasVal && i+1 < len(args) {
			val = args[i+1]
		}
		got = ""
		if v := strings.ToLower(val); sessionIDRE.MatchString(v) {
			got = v
		}
	}
	return got
}

// shortAttached is the single-dash name that a (-r<value>) carries attached, "" when a is not one.
func shortAttached(a string, names []string) string {
	for _, n := range names {
		if len(n) == 2 && n[0] == '-' && n[1] != '-' && strings.HasPrefix(a, n) && len(a) > 2 && a[2] != '=' {
			return n
		}
	}
	return ""
}

// newSessionID is a random v4 UUID, the form claude's --session-id takes.
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// prepareSession derives opts from a resume's saved launch overlay (typed flags win), else names a new id.
func prepareSession(opts *agent.LaunchOptions, args []string) (rest []string, id string, pin *sessionRecord, err error) {
	if rid := flagArg(args, "-r", "--resume"); rid != "" {
		rec, readErr := readSessionRecord(rid)
		if readErr != nil && !os.IsNotExist(readErr) {
			return args, "", nil, &agent.UsageError{Msg: "session launch record: " + readErr.Error(), Err: readErr}
		}
		if readErr == nil {
			eff, deriveErr := resumeOverlay(rec.ov, *opts)
			if deriveErr != nil {
				return args, "", nil, &agent.UsageError{Msg: "session launch record: " + deriveErr.Error(), Err: deriveErr}
			}
			opts.Route, opts.Provider, opts.Model, opts.Resumed = eff.Route(), eff.Profile, eff.Model, true
			rec.ov, rec.fenceDoc = eff, sessionFenceDoc(eff, rec.fenceDoc)
			pin = &rec
		}
		return args, flagArg(args, "--session-id"), pin, nil // a fork names its own id
	}
	if id := flagArg(args, "--session-id"); id != "" {
		return args, id, nil, nil
	}
	if agent.IsHeadless(args) || slices.ContainsFunc(args, func(a string) bool {
		name, _, _ := strings.Cut(a, "=")
		return slices.Contains(noFreshID, name) || shortAttached(a, noFreshID) != ""
	}) {
		return args, "", nil, nil
	}
	id, err = newSessionID()
	if err != nil {
		return args, "", nil, nil
	}
	return append([]string{"--session-id", id}, args...), id, nil, nil
}

func resumeOverlay(base agentconfig.Overlay, opts agent.LaunchOptions) (agentconfig.Overlay, error) {
	var typed, flags agentconfig.Overlay
	var err error
	if opts.Overlay != "" {
		typed, err = agentconfig.LoadOverlayFrom(context.Background(), opts.Overlay, nil, true)
		if err != nil {
			return agentconfig.Overlay{}, err
		}
	}
	reg, err := providers.Load()
	if err != nil {
		return agentconfig.Overlay{}, err
	}
	if opts.ProfileFlag {
		row := reg.Row(opts.Provider)
		if row == nil {
			return agentconfig.Overlay{}, fmt.Errorf("unknown profile %q", opts.Provider)
		}
		flags.Provider, flags.Profile = row.Provider, row.Name
	}
	if opts.ModelFlag {
		flags.Model = opts.Model
	}
	eff, err := base.Derive(typed, agentconfig.HopResume)
	if err == nil {
		eff, _, err = agentconfig.ApplyOverlay(reg, nil, flags, agentconfig.Overlay{}, eff)
	}
	return eff, err
}

// sessionPlan builds an interactive launch: a resume derives from its overlay, fresh launches save it.
func sessionPlan(as string, opts *agent.LaunchOptions, args []string) (agent.Plan, error) {
	args, id, pin, err := prepareSession(opts, args)
	if err != nil {
		return agent.Plan{}, err
	}
	ctx := providers.WithIdentity(context.Background(), as)
	if opts.Resumed {
		if opts.Route.Endpoint != "" {
			ctx = agent.WithEndpoint(ctx, opts.Route.Endpoint)
		}
		if as == "" && opts.Route.Identity != "" {
			ctx = providers.WithIdentity(ctx, opts.Route.Identity)
		}
	}
	if pin != nil {
		opts.Values = pin.env()
		ctx = proc.WithValues(ctx, opts.Values)
	}
	plan, err := claudePlan(ctx, *opts, args)
	if err != nil {
		return plan, err
	}
	if pin != nil {
		plan.Values = opts.Values // withOverlay hands the child this snapshot's fence, not the shell's
	}
	if id != "" && !opts.PrintEnv {
		model := plan.Set["ANTHROPIC_MODEL"]
		if slices.Contains(plan.Keep, "ANTHROPIC_MODEL") {
			model = proc.Value(ctx, "ANTHROPIC_MODEL")
		}
		savedCtx, err := sessionOverlayContext(ctx, opts.Overlay)
		if err != nil {
			return agent.Plan{}, err
		}
		r := opts.Route
		if as != "" {
			r.Identity = as
		}
		if err := writeLaunchOverlay(id, buildSessionOverlay(savedCtx, plan.Set[providers.EnvSessionProfile], cmp.Or(model, opts.Model, agent.MainAlias(*opts)), r)); err != nil {
			return agent.Plan{}, fmt.Errorf("save session launch record: %w", err)
		}
	}
	return plan, nil
}

func sessionOverlayContext(ctx context.Context, typed string) (context.Context, error) {
	if typed == "" {
		return ctx, nil
	}
	base, err := agentconfig.LoadOverlayFrom(ctx, "", nil, false)
	if err != nil {
		return ctx, err
	}
	add, err := agentconfig.LoadOverlayFrom(ctx, typed, nil, true)
	if err != nil {
		return ctx, err
	}
	merged, err := base.Derive(add, agentconfig.HopAppend)
	if err != nil {
		return ctx, err
	}
	return proc.WithValues(ctx, proc.Values{route.OverlayVar: merged.Doc()}), nil
}

// buildSessionOverlay builds the saved launch overlay: resolved unit, model, and inherited fence/caps.
func buildSessionOverlay(ctx context.Context, profile, model string, r route.Route) agentconfig.Overlay {
	ov := agentconfig.Overlay{Model: model}
	ov.Endpoint, ov.Provider, ov.Profile, ov.Identity, ov.Account = r.Endpoint, r.Provider, profile, r.Identity, r.Account
	if ov.Provider == "" && profile != "" {
		if reg, err := providers.Load(); err == nil {
			if row := reg.Row(profile); row != nil {
				ov.Provider = row.Provider
			}
		}
	}
	if inh, err := agentconfig.LoadOverlayContext(ctx, "", nil); err == nil {
		ov.MaxChildren, ov.MaxTreeTokens, ov.MaxDepth, ov.Runner = inh.MaxChildren, inh.MaxTreeTokens, inh.MaxDepth, inh.Runner
		if inh.Fence != nil {
			f := *inh.Fence
			ov.Fence = &f
		} else if p, prof, _ := router.OverlayRoute(ctx); p != "" && prof != "" {
			ov.Fence = &route.Fence{Profiles: []string{prof}}
		}
	}
	return ov
}

// env drops the terminal's endpoint and swaps its overlay for the record's fence; a live dispatch's overlay stays.
func (f sessionRecord) env() proc.Values {
	v := proc.ValuesFromEnv(os.Environ())
	v[proc.EnvAnthropicBaseURL] = ""
	if v[proc.DispatchIDEnv] == "" || v[route.OverlayVar] == "" {
		v[route.OverlayVar] = f.fenceDoc
	}
	if v[route.OverlayVar] == "" {
		delete(v, route.OverlayVar)
	}
	return v
}

// NoteSession records the launch overlay of a session the launcher never named (/clear, a fork) from the hook's env.
func NoteSession(id string) {
	profile := os.Getenv(providers.EnvSessionProfile)
	if profile == "" || !sessionIDRE.MatchString(id) {
		return
	}
	if err := writeLaunchOverlay(id, buildSessionOverlay(context.Background(), profile, os.Getenv("ANTHROPIC_MODEL"), route.Route{})); err != nil {
		fmt.Fprintf(os.Stderr, "save session launch record: %v\n", err)
	}
}

// readFacts returns id's saved launch overlay when its profile is still in the registry.
func readFacts(id string) (agentconfig.Overlay, bool) {
	rec, err := readSessionRecord(id)
	return rec.ov, err == nil
}

func readSessionRecord(id string) (sessionRecord, error) {
	data, err := os.ReadFile(factsPath(id))
	if err != nil {
		return sessionRecord{}, err
	}
	var ov agentconfig.Overlay
	var fenceDoc string
	var header struct {
		Provider string          `json:"provider"`
		Endpoint json.RawMessage `json:"endpoint"`
		Identity json.RawMessage `json:"identity"`
		Account  json.RawMessage `json:"account"`
		Fence    json.RawMessage `json:"fence"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return sessionRecord{}, err
	}
	if header.Provider == "" && (len(header.Endpoint) > 0 || len(header.Identity) > 0 || len(header.Account) > 0) {
		return sessionRecord{}, fmt.Errorf("provider is missing from the recorded route")
	}
	if header.Provider == "" || len(header.Fence) > 0 && header.Fence[0] == '"' {
		ov, fenceDoc, err = readLegacyLaunch(data)
	} else {
		ov, err = agentconfig.LoadOverlayFrom(context.Background(), string(data), nil, false)
	}
	if err != nil {
		return sessionRecord{}, err
	}
	reg, err := providers.Load()
	if err != nil {
		return sessionRecord{}, err
	}
	row, err := reg.Get(ov.Profile)
	if err != nil {
		return sessionRecord{}, err
	}
	if ov.Provider == "" {
		ov.Provider = row.Provider
	} else if ov.Provider != row.Provider {
		return sessionRecord{}, fmt.Errorf("profile %s no longer belongs to provider %s", ov.Profile, ov.Provider)
	}
	return sessionRecord{ov: ov, fenceDoc: sessionFenceDoc(ov, fenceDoc)}, nil
}

func readLegacyLaunch(data []byte) (agentconfig.Overlay, string, error) {
	var legacy legacyLaunchFacts
	if err := json.Unmarshal(data, &legacy); err != nil || legacy.Profile == "" {
		return agentconfig.Overlay{}, "", fmt.Errorf("invalid legacy record")
	}
	ov := agentconfig.Overlay{Profile: legacy.Profile, Model: legacy.Model}
	if legacy.Fence == "" {
		return ov, "", nil
	}
	old, err := agentconfig.LoadOverlayFrom(context.Background(), legacy.Fence, nil, false)
	if err != nil {
		return ov, "", err
	}
	ov.MaxChildren, ov.MaxTreeTokens, ov.MaxDepth, ov.Runner = old.MaxChildren, old.MaxTreeTokens, old.MaxDepth, old.Runner
	ov.Fence = old.Fence
	if ov.Fence == nil && old.Profile != "" {
		ov.Fence = &route.Fence{Profiles: []string{old.Profile}}
	}
	return ov, legacy.Fence, nil
}

func sessionFenceDoc(ov agentconfig.Overlay, legacy string) string {
	if ov.Fence == nil && ov.MaxChildren == nil && ov.MaxTreeTokens == nil && ov.MaxDepth == nil && ov.Runner == "" {
		return legacy
	}
	child := agentconfig.Overlay{Fence: ov.Fence, MaxChildren: ov.MaxChildren, MaxTreeTokens: ov.MaxTreeTokens,
		MaxDepth: ov.MaxDepth, Runner: ov.Runner}
	if ov.Fence != nil {
		child.Provider, child.Profile = ov.Provider, ov.Profile
	}
	return child.Doc()
}

// writeLaunchOverlay keeps the first recorded launch for a session.
func writeLaunchOverlay(id string, ov agentconfig.Overlay) error {
	path := factsPath(id)
	if ov.Profile == "" {
		return fmt.Errorf("profile is empty")
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := paths.AtomicWrite(path, data, 0o600); err != nil {
		return err
	}
	pruneFacts(filepath.Dir(path))
	return nil
}

// pruneEvery spaces the sweeps and is the grace a record gets before its transcript must exist.
const pruneEvery = 24 * time.Hour

// pruneFacts drops records whose transcript is gone, at most daily; records are ~100 bytes, so a resumable one stays.
func pruneFacts(dir string) {
	stamp := filepath.Join(dir, ".pruned")
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < pruneEvery {
		return
	}
	if paths.AtomicWrite(stamp, nil, 0o600) != nil {
		return
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		id := strings.TrimSuffix(e.Name(), ".json")
		fi, err := e.Info()
		if err != nil || !sessionIDRE.MatchString(id) || time.Since(fi.ModTime()) < pruneEvery {
			continue
		}
		if m, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", id+".jsonl")); len(m) == 0 {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
