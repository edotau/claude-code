package waves

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// Spec supplies one package's brief and batch routing, with ID expressed as W.k.
type Spec struct {
	ID, Agent, Profile, Task, Workdir, Verify string
	Owns                                      []string
}

// LaunchOptions supplies wave-wide routing and the memory reports directory.
type LaunchOptions struct {
	Slug, Provider, ReportsDir string
	Profiles                   []string
	Route                      []string // ask.RouteFlags.Args(): overlay, fallback, mode, allow, ask, deny
	Tools                      *bool
	Timeout                    time.Duration
}

// Envelope associates a batch outcome and saved result with its package.
type Envelope struct {
	TaskID     string `json:"task_id"`
	DispatchID string `json:"dispatch_id"`
	Status     string `json:"status"`
	Report     string `json:"-"`
	Result     string `json:"-"`
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

var launchID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
var launchPhase = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// Launch runs one ask batch, returning input-ordered envelopes even when a child leaf fails.
func Launch(ctx context.Context, executable string, specs []Spec, options LaunchOptions) ([]Envelope, error) {
	lines, err := launchLines(specs, options)
	if err != nil || len(specs) == 0 {
		return nil, err
	}
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("launch executable: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, executable, askArgs(len(specs), options)...)
	cmd.Stdin = bytes.NewReader(lines)
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, os.Stderr
	runErr := cmd.Run()
	envelopes, parseErr := launchEnvelopes(stdout.Bytes(), specs, options)
	if parseErr != nil {
		return nil, errors.Join(runErr, parseErr)
	}
	return envelopes, errors.Join(runErr, launchSave(ctx, executable, envelopes))
}

// askArgs is the `agents ask` batch command line (without the executable) for n leaves.
func askArgs(n int, options LaunchOptions) []string {
	args := []string{"agents", "ask", "--batch", "-", "--jobs", strconv.Itoa(min(n, 24)), "--format", "json"}
	if options.Provider != "" {
		args = append(args, "--provider", options.Provider)
	}
	return append(args, options.Route...)
}

// LeafLaunch is what Launch hands one leaf built from options and an owned file with a verify command:
// the ask arguments, the line's tools setting (nil = the batch default) and the brief text.
func LeafLaunch(options LaunchOptions, owned, verify string) (args []string, tools *bool, brief string, err error) {
	line, err := launchLineFor(0, "leaf-1.1", Spec{ID: "1.1", Task: "task", Owns: []string{owned}, Verify: verify},
		LaunchOptions{ReportsDir: "reports", Provider: options.Provider, Tools: options.Tools})
	if err != nil {
		return nil, nil, "", err
	}
	return askArgs(1, options), line.Tools, line.Prompt, nil
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
		if !launchID.MatchString(id) || !launchPhase.MatchString(spec.ID) || seen[id] {
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
	report := filepath.Join(options.ReportsDir, id+".md")
	prompt, err := agent.RenderBrief(agent.BriefInput{Task: spec.Task, Owned: spec.Owns, Verify: spec.Verify, Report: report})
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
		Tools: options.Tools, PlanTask: "Phase " + spec.ID}
	if options.Timeout > 0 {
		line.Timeout = options.Timeout.String()
	}
	return line, nil
}

func launchEnvelopes(data []byte, specs []Spec, options LaunchOptions) ([]Envelope, error) {
	wanted := make(map[string]int, len(specs))
	for i, spec := range specs {
		wanted[options.Slug+"-"+spec.ID] = i
	}
	out := make([]Envelope, len(specs))
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var envelope Envelope
		err := decoder.Decode(&envelope)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode batch envelope: %w", err)
		}
		i, ok := wanted[envelope.TaskID]
		if !ok || out[i].TaskID != "" {
			return nil, fmt.Errorf("unexpected batch task %q", envelope.TaskID)
		}
		if err := launchCheckEnvelope(envelope); err != nil {
			return nil, err
		}
		envelope.Report = filepath.Join(options.ReportsDir, envelope.TaskID+".md")
		envelope.Result = filepath.Join(options.ReportsDir, envelope.TaskID+".result.txt")
		out[i] = envelope
	}
	for _, envelope := range out {
		if envelope.TaskID == "" {
			return nil, errors.New("batch omitted a task envelope")
		}
	}
	return out, nil
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
