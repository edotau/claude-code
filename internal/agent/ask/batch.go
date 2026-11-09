package ask

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent"
)

// DefaultToolsTimeout is the deadline of a fresh --tools run without an explicit timeout.
const DefaultToolsTimeout = 45 * time.Minute

// TaskLine is one `ask --batch` JSONL task; Tools stays nil when absent so the flag default applies.
type TaskLine struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Profile string `json:"profile"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Workdir string `json:"workdir"`
	Tools   *bool  `json:"tools"`
	Timeout string `json:"timeout"` // a Go duration such as 5m

	PlanTask string `json:"plan_task"` // optional plan task this line implements, same character rule as id
}

// ParsedLine is one non-blank input line: its 1-based number, the decoded task, or why it did not decode.
type ParsedLine struct {
	N    int
	Task TaskLine
	Err  error
}

// ReadTaskLines decodes JSONL tasks; a malformed line or unknown field is that line's Err, never the whole batch's.
func ReadTaskLines(r io.Reader) ([]ParsedLine, error) {
	var out []ParsedLine
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, decodeTaskLine(line, n))
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// decodeTaskLine decodes one JSONL line that is physical line n; a bad line keeps n in its Err.
func decodeTaskLine(line []byte, n int) ParsedLine {
	pl := ParsedLine{N: n}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if derr := dec.Decode(&pl.Task); derr != nil {
		pl.Err = fmt.Errorf("line %d: %w", n, derr)
	} else if extra := (json.RawMessage)(nil); !errors.Is(dec.Decode(&extra), io.EOF) {
		pl.Err = fmt.Errorf("line %d: trailing data after the JSON object", n) // More() misses a stray '}'
	}
	return pl
}

// Task is one scheduled task; Prepare routes it when it is about to start, and Err fails it before any run.
type Task struct {
	ID      string
	Agent   string // the agent asked for, named on a failure envelope
	Plan    string // plan_task of the line, stamped on the dispatch record
	Req     agent.RunOptions
	Prepare func(ctx context.Context) (*client.Prepared, error)
	Retry   func(ctx context.Context) (*client.Prepared, error) // optional: reroutes a failed task once, same id
	Err     error
}

// NewID names one batch run.
func NewID() string {
	var b [3]byte
	_, _ = rand.Read(b[:]) // a failed read leaves zeros: the stamp alone still names the batch
	return "batch-" + batchNow().UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(b[:])
}

// batchNow is the clock NewID reads; a var so tests can force two batches onto one millisecond.
var batchNow = time.Now

var batchIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// planTaskRE is batchIDRE plus the space, so a plan heading such as "Phase 1.1" is a valid plan_task.
var planTaskRE = regexp.MustCompile(`^[A-Za-z0-9._ -]{1,64}$`)

// ParseTask validates one line like the ask flags; on error the task keeps its id and agent.
func ParseTask(l ParsedLine, agentName string, base agent.RunOptions, timeoutSet bool, cwd string,
	seen map[string]bool) (Task, error) {
	t := Task{ID: cmp.Or(l.Task.ID, "t"+strconv.Itoa(l.N)), Agent: cmp.Or(l.Task.Agent, agentName)}
	if l.Err != nil {
		return t, l.Err
	}
	tl := l.Task
	if !batchIDRE.MatchString(t.ID) {
		return t, fmt.Errorf("line %d: id %q must match %s", l.N, t.ID, batchIDRE)
	}
	if seen[t.ID] {
		return t, fmt.Errorf("line %d: duplicate id %q", l.N, t.ID)
	}
	if tl.PlanTask != "" && !planTaskRE.MatchString(tl.PlanTask) {
		return t, fmt.Errorf("line %d: plan_task %q must match %s", l.N, tl.PlanTask, planTaskRE)
	}
	t.Plan = tl.PlanTask
	seen[t.ID] = true
	if strings.TrimSpace(tl.Prompt) == "" {
		return t, fmt.Errorf("line %d: no task: give a prompt", l.N)
	}
	req := base
	req.Prompt, req.Provider, req.Model = tl.Prompt, cmp.Or(tl.Profile, base.Provider), cmp.Or(tl.Model, base.Model)
	name, err := client.SelectAndOverlay(t.Agent, base.Agents, &req) // explicit line/base options win over the definition
	if err != nil {
		return t, fmt.Errorf("line %d: %w", l.N, err)
	}
	t.Agent = name
	if tl.Tools != nil {
		req.Tools = *tl.Tools
	}
	if req.Tools && req.NoTools {
		return t, fmt.Errorf("line %d: tools and --no-tools are mutually exclusive", l.N)
	}
	req, err = batchTaskLocation(l, req, timeoutSet, base.WorkDir, cwd)
	if err != nil {
		return t, err
	}
	t.Req = req
	return t, nil
}

func batchTaskLocation(l ParsedLine, req agent.RunOptions, timeoutSet bool, baseWorkdir, cwd string) (agent.RunOptions, error) {
	switch {
	case l.Task.Timeout != "":
		d, err := time.ParseDuration(l.Task.Timeout)
		if err != nil || d <= 0 {
			return req, fmt.Errorf("line %d: timeout %q must be a positive duration such as 5m", l.N, l.Task.Timeout)
		}
		req.Timeout = d
	case req.Tools && !timeoutSet:
		req.Timeout = DefaultToolsTimeout
	}
	req.WorkDir = cmp.Or(l.Task.Workdir, baseWorkdir, cwd)
	abs, err := filepath.Abs(req.WorkDir)
	if err != nil {
		return req, fmt.Errorf("line %d: workdir %q: %w", l.N, req.WorkDir, err)
	}
	req.WorkDir = abs
	return req, nil
}
