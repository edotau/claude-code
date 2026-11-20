package waves

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/parallel"
)

// Spec supplies one package's brief and batch routing, with ID expressed as W.k.
type Spec struct {
	ID, Agent, Profile, Task, Workdir, Verify string
	Owns                                      []string
	Scope                                     string // dry-run only: scoped or detect-only
}

// LaunchOptions supplies wave-wide routing and the memory reports directory.
type LaunchOptions struct {
	Slug, Provider, ReportsDir string
	Day                        time.Time // the run's day for report names; zero means now
	Profiles                   []string
	Route                      []string // ask.RouteFlags.Args(): overlay, fallback, mode, allow, ask, deny
	Tools                      *bool
	Timeout                    time.Duration
	Wave                       []Spec          // every package of the wave: the files a leaf must not write
	Fingerprint                FingerprintFunc // hashes an owned path; a leaf run alone is checked on the others'
}

// Envelope associates a batch outcome and saved result with its package.
type Envelope struct {
	TaskID     string `json:"task_id"`
	DispatchID string `json:"dispatch_id"`
	Status     string `json:"status"`
	Report     string `json:"-"`
	Result     string `json:"-"`
	Scoped     bool   `json:"-"` // the leaf launched with its writes confined to its own files
	// Foreign are other packages' paths that changed while this leaf ran alone, so its own writes.
	Foreign []CheckViolation `json:"-"`
}

type launchLine struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	Profile  string `json:"profile"`
	Prompt   string `json:"prompt"`
	Workdir  string `json:"workdir"`
	Tools    *bool  `json:"tools,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	PlanTask string `json:"plan_task"`
}

var launchID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`) })
var launchPhase = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]+\.[0-9]+$`) })

// Launch returns input-ordered envelopes, retaining valid siblings when a batch fails.
func Launch(ctx context.Context, executable string, specs []Spec, options LaunchOptions) ([]Envelope, error) {
	if len(specs) == 0 {
		_, err := launchLines(nil, options)
		return nil, err
	}
	if _, err := launchLines(specs, options); err != nil {
		return nil, err
	}
	if executable == "" {
		var err error
		if executable, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("launch executable: %w", err)
		}
	}
	out, runErr := make([]Envelope, len(specs)), error(nil)
	for _, res := range launchAll(ctx, executable, specs, options) {
		runErr = errors.Join(runErr, containPanic(res, specs), res.snapErr)
		for j, e := range res.envelopes {
			out[res.batch.idx[j]], out[res.batch.idx[j]].Scoped = e, res.batch.enforced
			out[res.batch.idx[j]].Foreign = res.foreign
		}
	}
	out = slices.DeleteFunc(out, func(e Envelope) bool { return e.TaskID == "" })
	if len(out) == 0 {
		out = nil
	}
	return out, runErr
}

// containPanic keeps a panic's value and stack, which the run's persisted reason must not carry, on stderr.
func containPanic(res batchResult, specs []Spec) error {
	if _, ok := errors.AsType[*parallel.PanicError](res.err); !ok {
		return res.err
	}
	fmt.Fprintln(os.Stderr, res.err)
	ids := make([]string, len(res.batch.idx))
	for j, i := range res.batch.idx {
		ids[j] = specs[i].ID
	}
	return fmt.Errorf("leaves %s: launcher panicked (stack on stderr)", strings.Join(ids, ","))
}

// launchBatchRun runs one `agents ask --batch` over specs with the extra scope flags, saving each leaf's result.
func launchBatchRun(ctx context.Context, executable string, specs []Spec, scope []string, options LaunchOptions) ([]Envelope, error) {
	lines, err := launchLines(specs, options)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, executable, askArgs(len(specs), options, scope)...)
	cmd.Stdin = bytes.NewReader(lines)
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, os.Stderr
	runErr := cmd.Run()
	envelopes, parseErr := launchEnvelopes(stdout.Bytes(), specs, options)
	return envelopes, errors.Join(runErr, parseErr, launchSave(ctx, executable, envelopes))
}

// askArgs is the `agents ask` batch command line (without the executable) for n leaves, scope flags last.
func askArgs(n int, options LaunchOptions, scope []string) []string {
	args := []string{"agents", "ask", "--batch", "-", "--jobs", strconv.Itoa(min(n, 24)), "--format", "json"}
	if options.Provider != "" {
		args = append(args, "--provider", options.Provider)
	}
	return append(append(args, options.Route...), scope...)
}

// LeafLaunch exposes the actual ask arguments, tools setting and rendered brief for the permission doctor.
func LeafLaunch(options LaunchOptions, owned, verify string) (args []string, tools *bool, brief string, err error) {
	spec := Spec{ID: "1.1", Agent: "opencode", Task: "task", Owns: []string{owned}, Verify: verify}
	sibling := Spec{ID: "1.2", Agent: "opencode", Owns: []string{owned + ".sibling"}}
	options.Wave, options.ReportsDir = []Spec{spec, sibling}, "reports" // a leaf with one sibling shows the scope flags
	line, err := launchLineFor(0, "leaf-1.1", spec, options)
	if err != nil {
		return nil, nil, "", err
	}
	scope, _ := scopeFlags(spec, options)
	return askArgs(1, options, scope), line.Tools, line.Prompt, nil
}

func launchSave(ctx context.Context, executable string, envelopes []Envelope) error {
	var saveErr error
	for _, envelope := range envelopes {
		if envelope.DispatchID == "" {
			continue
		}
		save := exec.CommandContext(ctx, executable, "agents", "result", envelope.DispatchID, "--save", envelope.Result, "--format", "json")
		save.Stderr = os.Stderr
		summary, err := save.Output()
		if err != nil && (envelope.Status == "ok" || !launchSavedFailure(err, summary, envelope.Result)) {
			saveErr = errors.Join(saveErr, fmt.Errorf("save %s: %w", envelope.TaskID, err))
		}
	}
	return saveErr
}

func launchSavedFailure(err error, summary []byte, path string) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return false
	}
	var saved struct {
		Path string `json:"path"`
	}
	return json.Unmarshal(summary, &saved) == nil && saved.Path == path
}

func launchLines(specs []Spec, options LaunchOptions) ([]byte, error) {
	if options.ReportsDir == "" {
		return nil, errors.New("launch requires the memory reports directory")
	}
	if options.Timeout < 0 {
		return nil, errors.New("launch timeout must be positive")
	}
	var lines bytes.Buffer
	seen := make(map[string]bool, len(specs))
	for i, spec := range specs {
		id := options.Slug + "-" + spec.ID
		if !launchID().MatchString(id) || !launchPhase().MatchString(spec.ID) || seen[id] {
			return nil, fmt.Errorf("invalid or duplicate batch id %q", id)
		}
		seen[id] = true
		line, err := launchLineFor(i, id, spec, options)
		if err != nil {
			return nil, err
		}
		if err := json.NewEncoder(&lines).Encode(line); err != nil {
			return nil, err
		}
	}
	return lines.Bytes(), nil
}

func launchLineFor(i int, id string, spec Spec, options LaunchOptions) (launchLine, error) {
	report := filepath.Join(options.ReportsDir, dateFirst(id, options.Day)+".md")
	verify := spec.Verify
	if verify == "" {
		verify = DefaultVerify(spec.Owns)
	}
	prompt, err := agent.RenderBrief(agent.BriefInput{Task: spec.Task, Owned: spec.Owns, Verify: verify, Report: report})
	if err != nil {
		return launchLine{}, err
	}
	profile := spec.Profile
	if profile == "" && len(options.Profiles) > 0 {
		profile = options.Profiles[i%len(options.Profiles)]
	}
	if profile != "" && options.Provider != "" {
		profile = options.Provider + "·" + profile
	}
	line := launchLine{ID: id, Agent: spec.Agent, Profile: profile, Prompt: prompt, Workdir: spec.Workdir,
		Tools: launchTools(options), PlanTask: "Phase " + spec.ID}
	if options.Timeout > 0 {
		line.Timeout = options.Timeout.String()
	}
	return line, nil
}

func launchTools(o LaunchOptions) *bool {
	if o.Tools != nil {
		return o.Tools
	}
	for _, arg := range o.Route {
		flag, value, hasValue := strings.Cut(arg, "=")
		if flag == "--tools" && hasValue {
			enabled := value != "false"
			return &enabled
		}
		if (flag == "--no-tools" || flag == "--read-only" || flag == "--autonomous") && value != "false" {
			return nil
		}
	}
	enabled := true
	return &enabled
}

func launchEnvelopes(data []byte, specs []Spec, options LaunchOptions) ([]Envelope, error) {
	wanted := make(map[string]int, len(specs))
	out := make([]Envelope, len(specs))
	for i, spec := range specs {
		id := options.Slug + "-" + spec.ID
		wanted[id] = i
		out[i] = Envelope{TaskID: id, Status: "error",
			Report: filepath.Join(options.ReportsDir, dateFirst(id, options.Day)+".md"),
			Result: filepath.Join(options.ReportsDir, dateFirst(id, options.Day)+".result.txt")}
	}
	var parseErr error
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var envelope Envelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			parseErr = errors.Join(parseErr, fmt.Errorf("decode batch envelope: %w", err))
			continue
		}
		i, ok := wanted[envelope.TaskID]
		if !ok {
			parseErr = errors.Join(parseErr, fmt.Errorf("unexpected batch task %q", envelope.TaskID))
			continue
		}
		delete(wanted, envelope.TaskID)
		if err := launchCheckEnvelope(envelope); err != nil {
			parseErr = errors.Join(parseErr, err)
			envelope.Status, envelope.DispatchID = "error", ""
		}
		envelope.Report, envelope.Result = out[i].Report, out[i].Result
		out[i] = envelope
	}
	for _, spec := range specs {
		if _, missing := wanted[options.Slug+"-"+spec.ID]; missing {
			parseErr = errors.Join(parseErr, fmt.Errorf("batch omitted task envelope %s", spec.ID))
		}
	}
	return out, parseErr
}

func launchCheckEnvelope(envelope Envelope) error {
	if !slices.Contains([]string{"ok", "error", "partial", "quota", "stalled"}, envelope.Status) {
		return fmt.Errorf("invalid status for %s", envelope.TaskID)
	}
	if envelope.Status == "ok" && envelope.DispatchID == "" {
		return fmt.Errorf("missing dispatch for %s", envelope.TaskID)
	}
	return nil
}

// DefaultVerify is the diff check over the owned paths.
func DefaultVerify(owns []string) string {
	var cmd strings.Builder
	cmd.WriteString("claude-code agents diff --check")
	for _, path := range owns {
		cmd.WriteString(" --path '" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'")
	}
	return cmd.String()
}

var leadingDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)

// dateFirst prefixes the run's day (default now) to a name unless it already starts with a date.
func dateFirst(name string, day time.Time) string {
	if leadingDate.MatchString(name) {
		return name
	}
	if day.IsZero() {
		day = time.Now()
	}
	return day.Format("2006-01-02") + "-" + name
}
