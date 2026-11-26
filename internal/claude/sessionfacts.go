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

// launchFacts is what a session was launched on, kept per session id so a resume comes back on it.
type launchFacts struct {
	Profile string `json:"profile"`
	Model   string `json:"model,omitempty"`
	Fence   string `json:"fence,omitempty"` // the confinement doc in force at launch, a leaf's fence
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

// prepareSession fills opts from a resume's record (typed flags win), else names a new id; gap: --opt --resume
func prepareSession(opts *agent.LaunchOptions, args []string) (rest []string, id string, pin *launchFacts) {
	if rid := flagArg(args, "-r", "--resume"); rid != "" {
		if f, ok := readFacts(rid); ok {
			typed := opts.Provider != "" || opts.Model != "" || opts.Overlay != ""
			if opts.Provider == "" {
				opts.Provider = f.Profile
				opts.Model = cmp.Or(opts.Model, f.Model)
			}
			if !typed {
				pin = &f
			}
		}
		return args, flagArg(args, "--session-id"), pin // a fork names its own id
	}
	if id := flagArg(args, "--session-id"); id != "" {
		return args, id, nil
	}
	if agent.IsHeadless(args) || slices.ContainsFunc(args, func(a string) bool {
		name, _, _ := strings.Cut(a, "=")
		return slices.Contains(noFreshID, name) || shortAttached(a, noFreshID) != ""
	}) {
		return args, "", nil
	}
	id, err := newSessionID()
	if err != nil {
		return args, "", nil
	}
	return append([]string{"--session-id", id}, args...), id, nil
}

// sessionPlan is claudePlan for an interactive launch: a resume comes back on its record, a fresh launch is recorded.
func sessionPlan(as string, opts *agent.LaunchOptions, args []string) (agent.Plan, error) {
	args, id, pin := prepareSession(opts, args)
	ctx := providers.WithIdentity(context.Background(), as)
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
		writeFacts(id, launchFacts{Profile: plan.Set[providers.EnvSessionProfile], Model: cmp.Or(opts.Model, agent.MainAlias(*opts)), Fence: fenceOf(ctx)})
	}
	return plan, nil
}

// env drops the terminal's endpoint and swaps its overlay for the record's fence; a live dispatch's overlay stays.
func (f launchFacts) env() proc.Values {
	v := proc.ValuesFromEnv(os.Environ())
	v[proc.EnvAnthropicBaseURL] = ""
	if v[proc.DispatchIDEnv] == "" || v[route.OverlayVar] == "" {
		v[route.OverlayVar] = f.Fence
	}
	if v[route.OverlayVar] == "" {
		delete(v, route.OverlayVar)
	}
	return v
}

// fenceOf is the confinement doc ctx carries (provider, profile and inherited caps, never the env layer).
func fenceOf(ctx context.Context) string {
	provider, profile, _ := router.OverlayRoute(ctx)
	o, _ := agentconfig.LoadOverlayContext(ctx, "", nil)
	return agentconfig.InlineOverlay(provider, profile, o)
}

// NoteSession records the launch facts of a session the launcher never named (/clear, a fork) from the hook's env.
func NoteSession(id string) {
	profile := os.Getenv(providers.EnvSessionProfile)
	if profile == "" || !sessionIDRE.MatchString(id) {
		return
	}
	writeFacts(id, launchFacts{Profile: profile, Model: os.Getenv("ANTHROPIC_MODEL"), Fence: fenceOf(context.Background())})
}

// readFacts is id's record when it names a profile still in the registry; anything else falls back to today's launch.
func readFacts(id string) (launchFacts, bool) {
	var f launchFacts
	data, err := os.ReadFile(factsPath(id))
	if err != nil || json.Unmarshal(data, &f) != nil || f.Profile == "" {
		return f, false
	}
	reg, err := providers.Load()
	if err != nil {
		return f, false
	}
	_, err = reg.Get(f.Profile)
	return f, err == nil
}

// writeFacts stores what a session was started on; best effort, a failed write only costs the resume pin.
func writeFacts(id string, f launchFacts) {
	path := factsPath(id)
	if f.Profile == "" {
		return
	}
	if _, err := os.Stat(path); err == nil { // the first launch's facts stay the session's
		return
	}
	data, err := json.Marshal(f)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	if paths.AtomicWrite(path, data, 0o600) == nil {
		pruneFacts(filepath.Dir(path))
	}
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
