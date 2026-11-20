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
	"flag"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/askspec"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"
)

// batchLine is one `ask --batch` JSONL task; Tools stays nil when absent so the flag default applies.
type batchLine struct {
	ID      string          `json:"id"`
	Agent   string          `json:"agent"`
	Profile string          `json:"profile"`
	Overlay json.RawMessage `json:"overlay"`
	Model   string          `json:"model"`
	Prompt  string          `json:"prompt"`
	Workdir string          `json:"workdir"`
	Tools   *bool           `json:"tools"`
	Timeout string          `json:"timeout"` // a Go duration such as 5m

	PlanTask string `json:"plan_task"` // optional plan task this line implements, same character rule as id
}

// parsedLine is one non-blank input line: its 1-based number, the decoded task, or why it did not decode.
type parsedLine struct {
	N    int
	Task batchLine
	Err  error
}

// maxTaskLine caps one JSONL task line; a longer line fails alone.
const maxTaskLine = 1 << 20

// readTaskLines decodes JSONL tasks; a malformed line or unknown field is that line's Err, never the whole batch's.
func readTaskLines(r io.Reader) ([]parsedLine, error) {
	var out []parsedLine
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, tooLong, err := textutil.ReadLineCapped(br, maxTaskLine)
		if tooLong {
			out = append(out, parsedLine{N: n, Err: fmt.Errorf("line %d: over the %d byte line limit", n, maxTaskLine)})
		} else if len(bytes.TrimSpace(line)) > 0 {
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

// perLineKeys are batchLine's JSON keys: the spec keys a line may set itself.
var perLineKeys = sync.OnceValue(func() []string {
	var out []string
	for f := range reflect.TypeFor[batchLine]().Fields() {
		out = append(out, strings.Split(f.Tag.Get("json"), ",")[0])
	}
	return out
})

// checkLineKeys names the nearest valid key for a mistyped one and refuses a batch-wide spec key on the line.
func checkLineKeys(line []byte) error {
	s, ok := askspec.Current()
	var doc map[string]json.RawMessage
	if !ok || json.Unmarshal(line, &doc) != nil {
		return nil // the strict decode reports what is wrong
	}
	if err := s.CheckKeys(doc, perLineKeys()...); err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(doc)) {
		if _, inSchema := s.Lookup(k); inSchema && !slices.Contains(perLineKeys(), k) {
			return fmt.Errorf("%q is batch-wide: put --%s on the --batch command", k, k)
		}
	}
	return nil
}

// decodeTaskLine decodes one JSONL line that is physical line n; a bad line keeps n in its Err.
func decodeTaskLine(line []byte, n int) parsedLine {
	pl := parsedLine{N: n}
	if err := checkLineKeys(line); err != nil {
		_ = json.Unmarshal(line, &struct {
			ID *string `json:"id"`
		}{&pl.Task.ID}) // the failure envelope keeps the line's id
		pl.Err = fmt.Errorf("line %d: %w", n, err)
		return pl
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if derr := dec.Decode(&pl.Task); derr != nil {
		pl.Err = fmt.Errorf("line %d: %w", n, derr)
	} else if extra := (json.RawMessage)(nil); !errors.Is(dec.Decode(&extra), io.EOF) {
		pl.Err = fmt.Errorf("line %d: trailing data after the JSON object", n) // More() misses a stray '}'
	}
	return pl
}

// batchTask is one scheduled task; Prepare routes it when it is about to start, and Err fails it before any run.
type batchTask struct {
	ID      string
	Agent   string // the agent asked for, named on a failure envelope
	Plan    string // plan_task of the line, stamped on the dispatch record
	arm     *armRetry
	Req     agent.RunOptions
	Prepare func(ctx context.Context) (*agent.Prepared, error)
	Err     error
}

// newID names one batch run.
func newID() string {
	var b [3]byte
	_, _ = rand.Read(b[:]) // a failed read leaves zeros: the stamp alone still names the batch
	return "batch-" + batchNow().UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(b[:])
}

// batchNow is the clock newID reads; a var so tests can force two batches onto one millisecond.
var batchNow = time.Now

var batchIDRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`) })

// planTaskRE is batchIDRE plus the space, so a plan heading such as "Phase 1.1" is a valid plan_task.
var planTaskRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._ -]{1,64}$`) })

// parseTask validates one line like the ask flags; on error the task keeps its id and agent.
func parseTask(l parsedLine, agentName string, base agent.RunOptions, timeoutSet bool, cwd string,
	seen map[string]bool) (batchTask, error) {
	t := batchTask{ID: l.Task.ID, Agent: cmp.Or(l.Task.Agent, agentName)}
	if t.ID == "" { // a default id must not collide with an explicit id taken earlier
		t.ID = "t" + strconv.Itoa(l.N)
		for i := 2; seen[t.ID]; i++ {
			t.ID = "t" + strconv.Itoa(l.N) + "-" + strconv.Itoa(i)
		}
	}
	if l.Err != nil {
		return t, l.Err
	}
	tl := l.Task
	if err := checkLineIDs(l.N, t.ID, tl.PlanTask, seen); err != nil {
		return t, err
	}
	t.Plan = tl.PlanTask
	seen[t.ID] = true
	if strings.TrimSpace(tl.Prompt) == "" {
		return t, fmt.Errorf("line %d: no task: give a prompt", l.N)
	}
	req, name, err := lineRequest(l, t.Agent, base)
	t.Agent = cmp.Or(name, t.Agent) // "" only when the agent selection itself failed
	if err != nil {
		return t, err
	}
	req, err = batchTaskLocation(l, req, timeoutSet, base.WorkDir, cwd)
	if err != nil {
		return t, err
	}
	t.Req = req
	return t, nil
}

// checkLineIDs refuses a malformed or already seen id and a malformed plan_task on line n.
func checkLineIDs(n int, id, planTask string, seen map[string]bool) error {
	switch {
	case !batchIDRE().MatchString(id):
		return fmt.Errorf("line %d: id %q must match %s", n, id, batchIDRE())
	case seen[id]:
		return fmt.Errorf("line %d: duplicate id %q", n, id)
	case planTask != "" && !planTaskRE().MatchString(planTask):
		return fmt.Errorf("line %d: plan_task %q must match %s", n, planTask, planTaskRE())
	}
	return nil
}

// lineRequest is base with the line's prompt, route, model and tools over it, plus the agent it selects.
func lineRequest(l parsedLine, agentName string, base agent.RunOptions) (agent.RunOptions, string, error) {
	tl, req := l.Task, base
	req.Prompt, req.Provider, req.Model = tl.Prompt, cmp.Or(tl.Profile, base.Provider), cmp.Or(tl.Model, base.Model)
	_, auto := agent.SplitAuto(tl.Model)
	req.ModelTyped = (tl.Model != "" && !auto) || base.ModelTyped // a line's own model is as typed as a --model flag
	if err := lineRoute(l, &req); err != nil {
		return req, "", fmt.Errorf("line %d: %w", l.N, err)
	}
	name, err := agent.SelectAndOverlay(agentName, base.Agents, &req) // explicit line/base options win over the definition
	if err != nil {
		return req, "", fmt.Errorf("line %d: %w", l.N, err)
	}
	if tl.Tools != nil {
		req.Tools = *tl.Tools
	}
	req.Tools = req.Tools || req.ReadOnly // read-only tools are still tools, whatever the line said
	if req.Tools && req.NoTools {
		return req, name, fmt.Errorf("line %d: tools and --no-tools are mutually exclusive", l.N)
	}
	if err := agent.CheckCapabilities(name, req); err != nil {
		return req, name, fmt.Errorf("line %d: %w", l.N, err)
	}
	return req, name, nil
}

func lineRoute(l parsedLine, req *agent.RunOptions) error {
	tl := l.Task
	if tl.Profile != "" {
		req.Lead = ""
		req.Account = ""
	}
	fs, f := askFlagSet()
	if tl.Profile != "" {
		_ = fs.Set("profile", tl.Profile)
	}
	if tl.Model != "" {
		_ = fs.Set("model", tl.Model)
	}
	fr := flagRoute(f)
	if len(tl.Overlay) == 0 {
		if !fr.IsZero() {
			req.Route = fr
		}
		return nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(tl.Overlay), []byte("{")) {
		return errors.New("overlay: want a JSON object")
	}
	ov, err := agentconfig.LoadOverlayContext(context.Background(), string(tl.Overlay), nil)
	if err != nil {
		return err
	}
	return applyLineOverlay(req, fs, f, fr, ov)
}

func applyLineOverlay(req *agent.RunOptions, fs *flag.FlagSet, f *askFlags, fr route.Route, ov agentconfig.Overlay) error {
	if fr.IsZero() && f.profile != "" {
		fr = route.Route{Profile: f.profile} // still name a mistyped profile in the conflict
	}
	if err := flagConflict(fs, f, fr, ov); err != nil {
		return err
	}
	if ov.Provider != "" {
		req.Route, req.Provider, req.Account = ov.Route(), ov.Profile, ov.Account
		req.Lead = ""
	}
	req.Model, req.Effort = cmp.Or(f.model, ov.Model, req.Model), cmp.Or(ov.Effort, req.Effort)
	return nil
}

func batchTaskLocation(l parsedLine, req agent.RunOptions, timeoutSet bool, baseWorkdir, cwd string) (agent.RunOptions, error) {
	switch {
	case l.Task.Timeout != "":
		d, err := time.ParseDuration(l.Task.Timeout)
		if err != nil || d <= 0 {
			return req, fmt.Errorf("line %d: timeout %q must be a positive duration such as 5m", l.N, l.Task.Timeout)
		}
		req.Timeout = d
	case req.Tools && !timeoutSet:
		req.Timeout = agent.DefaultToolsTimeout
	}
	req.WorkDir = cmp.Or(l.Task.Workdir, baseWorkdir, cwd)
	abs, err := filepath.Abs(req.WorkDir)
	if err != nil {
		return req, fmt.Errorf("line %d: workdir %q: %w", l.N, req.WorkDir, err)
	}
	req.WorkDir = abs
	return req, nil
}
