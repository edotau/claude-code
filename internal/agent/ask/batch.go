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
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/route"
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
	Own     []string        `json:"own"`     // the spec's own, report and verify keys wrap Prompt as a leaf brief
	Report  string          `json:"report"`
	Verify  string          `json:"verify"`

	PlanTask string `json:"plan_task"` // optional plan task this line implements, same character rule as id
	Group    string `json:"group"`     // optional phase proxy scope id the line's dispatch routes through
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
	s := askSchema()
	var doc map[string]json.RawMessage
	if json.Unmarshal(line, &doc) != nil {
		return nil // the strict decode reports what is wrong
	}
	for _, k := range slices.Sorted(maps.Keys(doc)) {
		if slices.Contains(perLineKeys(), k) {
			continue
		}
		if field, inSchema := s.Lookup(k); inSchema {
			return fmt.Errorf("%q is batch-wide: put --%s on the --batch command", k, field.FlagName())
		}
		if n, ok := lineKeyNear(k); ok {
			return fmt.Errorf("batch key %q unknown: did you mean %q?", k, n)
		}
		return fmt.Errorf("batch key %q unknown (keys: %s)", k, strings.Join(perLineKeys(), ", "))
	}
	return nil
}

// lineKeyNear suggests only a key valid on a line; "task" is the common miss for "prompt".
func lineKeyNear(k string) (string, bool) {
	if k == "task" {
		return "prompt", true
	}
	return textutil.Nearest(k, perLineKeys())
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
	Group   string // phase proxy scope id of the line, "" for none
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
	if tl.Group != "" && !agent.ValidProxyGroup(tl.Group) {
		return t, fmt.Errorf("line %d: group %q must be 1-64 lower-case letters, digits, . _ -, starting alphanumeric", l.N, tl.Group)
	}
	t.Group = tl.Group
	seen[t.ID] = true
	if strings.TrimSpace(tl.Prompt) == "" {
		return t, fmt.Errorf("line %d: no task: give a prompt", l.N)
	}
	req, name, err := lineRequest(l, t.Agent, base)
	t.Agent = cmp.Or(name, t.Agent) // "" only when the agent selection itself failed
	if err != nil {
		return t, err
	}
	if req, err = batchTaskLocation(l, req, timeoutSet, base.WorkDir, cwd); err != nil {
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
	req.Agent = agentName
	auto := agent.IsAuto(tl.Model)
	req.ModelTyped = (tl.Model != "" && !auto) || (tl.Model == "" && base.ModelTyped) // a line's model wins, auto too
	if err := lineRoute(l, &req); err != nil {
		return req, "", fmt.Errorf("line %d: %w", l.N, err)
	}
	name, err := agent.SelectAndOverlay(cmp.Or(tl.Agent, req.Agent), req.Agents, &req)
	if err != nil {
		return req, "", fmt.Errorf("line %d: %w", l.N, err)
	}
	req.Tools, req.Verify = *cmp.Or(tl.Tools, &req.Tools), tl.Verify
	if req.Tools && req.NoTools {
		return req, name, fmt.Errorf("line %d: tools and --no-tools are mutually exclusive", l.N)
	}
	if err := lineCapable(name, &req); err != nil {
		return req, name, fmt.Errorf("line %d: %w", l.N, err)
	}
	tl.Prompt = req.Prompt
	if req.Prompt, err = lineBrief(tl, name, req); err != nil {
		return req, name, fmt.Errorf("line %d: %w", l.N, err)
	}
	return req, name, nil
}

// lineCapable checks the line's agent can run req; a code mode it cannot give runs off, noted.
func lineCapable(name string, req *agent.RunOptions) error {
	dropGoogleTools(name, &req.GoogleTools)
	if err := agent.CheckCapabilities(name, *req); err != nil {
		return err
	}
	if req.Codemode == "" {
		return nil
	}
	var err error
	req.Codemode, req.CodemodeReason, err = validateCodemode(req.Codemode, name, req.Runner, req.Tools, directRoute("", req.Provider))
	noteCodemodeOff(name, req.CodemodeReason)
	return err
}

// lineBrief is the line's prompt, wrapped as a leaf brief when it sets own, report or verify.
func lineBrief(tl batchLine, agentName string, base agent.RunOptions) (string, error) {
	var argv []string
	if agentName != "" {
		argv = append(argv, "--agent", agentName)
	}
	if base.Codemode != "" {
		argv = append(argv, "--codemode", base.Codemode)
	}
	argv = append(argv, "--tools="+strconv.FormatBool(*cmp.Or(tl.Tools, &base.Tools)))
	brief, _, err := specBrief(tl.Prompt, argv, Task{Own: tl.Own, Report: tl.Report, Verify: tl.Verify})
	return brief, err
}

// errColonForm marks a refused provider:profile value; a batch counts it as a usage error.
var errColonForm = errors.New("the provider:profile form is not accepted")

func lineRoute(l parsedLine, req *agent.RunOptions) error {
	tl := l.Task
	if strings.Contains(tl.Profile, ":") {
		return fmt.Errorf("profile: %w: %s", errColonForm, routeForms)
	}
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
		req.Direct = ov.Endpoint == endpointDirect
		req.Lead = ""
	}
	req.Model, req.Effort = cmp.Or(f.model, ov.Model, req.Model), cmp.Or(ov.Effort, req.Effort)
	if ov.Fallback != "" {
		req.Fallback, req.FallbackDefaulted = ov.Fallback, false
	}
	if f.model == "" && ov.Model != "" {
		req.ModelTyped = !agent.IsAuto(ov.Model) // the line overlay's model won: typed unless auto, so no fallback swaps it
	}
	if err := lineRunOptions(req, settleTools(ov)); err != nil {
		return err
	}
	req.Runner = cmp.Or(ov.Runner, req.Runner)
	req.Values = req.Values.Merge(nil)
	var held agentconfig.Overlay
	var err error
	if doc := req.Values[route.OverlayVar]; doc != "" {
		held, err = agentconfig.LoadOverlayFrom(context.Background(), doc, nil, false) // the carried doc is stored, not typed
	}
	if err != nil {
		return err
	}
	held, err = held.Derive(ov, agentconfig.HopAppend)
	if err != nil {
		return err
	}
	req.Values[route.OverlayVar] = held.Doc()
	return nil
}

// lineRunOptions applies the line's per-run keys before agent selection, capabilities and timeout defaults.
func lineRunOptions(req *agent.RunOptions, ov agentconfig.Overlay) error {
	strs, bools := runKeyFields(&ov)
	for name, dst := range map[string]*string{"agent": &req.Agent, "host": &req.Host, "codemode": &req.Codemode,
		"debug-file": &req.DebugFile, "system-prompt": &req.SystemPrompt, "append-system-prompt": &req.AppendSystemPrompt} {
		if v := *strs[name]; v != "" {
			*dst = v
		}
	}
	for name, dst := range map[string]*bool{"tools": &req.Tools, "no-tools": &req.NoTools, "no-persist": &req.NoPersist,
		"native": &req.Native, "autonomous": &req.Autonomous, "fleet": &req.Fleet, "enable-memory": &req.EnableMemory} {
		if v := *bools[name]; v != nil {
			*dst = *v
		}
	}
	var err error
	if ov.Timeout != "" {
		if req.Timeout, err = time.ParseDuration(ov.Timeout); err != nil {
			return err
		}
		req.TimeoutTyped = true
	}
	if len(ov.Agents) > 0 {
		if req.Agents, err = parseAgentDefs(string(ov.Agents), req.Host); err != nil {
			return err
		}
	}
	if ov.OutputSchema != "" {
		if req.OutputSchema, err = resolveOutputSchema(ov.OutputSchema); err != nil {
			return err
		}
	}
	if ov.GoogleTools != "" {
		req.GoogleTools, err = googleToolNames(ov.GoogleTools)
	}
	req.Prompt = agent.WithTier(req.Prompt, ov.Tier)
	return err
}

func batchTaskLocation(l parsedLine, req agent.RunOptions, timeoutSet bool, baseWorkdir, cwd string) (agent.RunOptions, error) {
	switch {
	case l.Task.Timeout != "":
		d, err := time.ParseDuration(l.Task.Timeout)
		if err != nil || d <= 0 {
			return req, fmt.Errorf("line %d: timeout %q must be a positive duration such as 5m", l.N, l.Task.Timeout)
		}
		req.Timeout, req.TimeoutTyped = d, true
	case req.Tools && !timeoutSet && !req.TimeoutTyped:
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
