package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// FormatStreamJSON is the --format value for the newline-delimited event stream.
const FormatStreamJSON = "stream-json"

// ValidateAs validates --as; it applies to claude only, other agents follow the profile's identity setting.
func ValidateAs(as, ag string) error {
	if as == "" {
		return nil
	}
	if as != providers.IdentityUser && as != providers.IdentitySP {
		return fmt.Errorf("--as %q must be user or sp", as)
	}
	if ag != "claude" {
		return fmt.Errorf("--as applies to claude only; %s follows the profile's identity setting", ag)
	}
	return nil
}

// ValidateAskFlags checks --format and --fallback against their allowed values.
func ValidateAskFlags(format, fallback string) error {
	if !slices.Contains([]string{helptext.Text, helptext.JSON, FormatStreamJSON}, format) {
		return fmt.Errorf("--format %q unknown (text|json|stream-json)", format)
	}
	if !slices.Contains(router.FallbackModes, fallback) {
		return fmt.Errorf("--fallback %q unknown (%s)", fallback, strings.Join(router.FallbackModes, "|"))
	}
	return nil
}

// AskExclusive rejects flag pairs that cannot be combined.
func AskExclusive(background bool, job string, tools, noTools bool) error {
	if background && job != "" {
		return errors.New("--background and --job are mutually exclusive")
	}
	if tools && noTools {
		return errors.New("--tools and --no-tools are mutually exclusive")
	}
	return nil
}

// AskPositional is the task words; on resume the first positional sets session.
func AskPositional(positional []string, resume bool, session *string) ([]string, error) {
	if !resume {
		return positional, nil
	}
	if len(positional) < 1 {
		return nil, errors.New("resume needs a session id: <agent> resume [flags] <id> <task>")
	}
	*session = positional[0]
	return positional[1:], nil
}

// AskReselect re-resolves the agent after --job/--retry; it returns the new selector and agent.
func AskReselect(ref, ag string, name *string, persona **Schema, defs map[string]*Schema) (string, string, error) {
	if *name != ag { // --job/--retry replaced the agent: resolve the record's (or re-typed) value instead
		ref = *name
	}
	rest, p, err := SelectAgent(ref, defs) // a --retry re-parse puts the raw --agent value back
	if err != nil {
		return ref, ag, err
	}
	*name = rest
	if p != nil { // an explicit persona beats the record's
		*persona = p
	}
	return ref, rest, nil
}

// ReadFramedPrompt reads the prompt up to the NUL that ends it and returns the reader, whose remaining EOF is the cancel signal.
func ReadFramedPrompt(in io.Reader) (*bufio.Reader, string, error) {
	rd := bufio.NewReader(in)
	p, err := rd.ReadString(0)
	if err != nil {
		return nil, "", fmt.Errorf("--stdin-framed: stdin ended before the NUL that ends the prompt: %w", err)
	}
	return rd, strings.TrimSuffix(p, "\x00"), nil
}

// ResolveAskPrompt reads stdin when the prompt is empty or the literal "-".
func ResolveAskPrompt(prompt string) (string, error) {
	if prompt != "" && prompt != "-" {
		return prompt, nil
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(raw), nil
}

// BuildAskRecord constructs the dispatch record for req, reusing jobRec's identity when resuming a job.
func BuildAskRecord(ctx context.Context, name string, req RunOptions, jobRec *Record, workdir string, background bool) Record {
	if jobRec != nil {
		return *jobRec // same id: a job run continues the record it was spawned from
	}
	rec := ClaimRecord(name, req, time.Now())
	cwd := workdir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	rec.Repo = RepoRoot(ctx, cwd)
	rec.Owner = inflight.SessionOwner()
	rec.Log = rec.LogPath()
	if !background {
		rec.PID = os.Getpid() // foreground run: alive while this process runs, lost only if it dies
	}
	return rec
}

// RepoRoot is the main checkout root for cwd, falling back to cwd when git fails.
func RepoRoot(ctx context.Context, cwd string) string {
	out, err := proc.RunGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return cwd
	}
	return paths.MainCheckout(strings.TrimSpace(out))
}

// ResolveOutputSchema reads --output-schema as inline JSON when it starts with "{", else as a file, and checks it parses.
func ResolveOutputSchema(arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	body := strings.TrimSpace(arg)
	if !strings.HasPrefix(body, "{") {
		data, err := os.ReadFile(arg)
		if err != nil {
			return "", fmt.Errorf("--output-schema: %w", err)
		}
		body = strings.TrimSpace(string(data))
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return "", fmt.Errorf("--output-schema: not a JSON object: %w", err)
	}
	return body, nil
}

// RecordEnvelope is the result event's envelope, or one rebuilt from rec when no result event can be read.
func RecordEnvelope(rec Record, errw io.Writer) Envelope {
	if f, err := os.Open(rec.EventsPath()); err == nil {
		defer f.Close()
		var env *Envelope
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == EventResult && ev.Envelope != nil {
				env = ev.Envelope
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(errw, "agents result: %s: events unreadable, envelope may be incomplete: %v\n", rec.ID, err)
		}
		if env != nil {
			return *env
		}
	}
	var runErr error
	if rec.Status == "error" {
		runErr = fmt.Errorf("%s", rec.Error)
	}
	res := Result{Agent: rec.Agent, Provider: rec.Provider, Model: rec.Model, Invoked: rec.Invoked,
		SessionID: rec.SessionID, DurationMs: rec.DurationMs, Fallback: rec.Fallback, FallbackReason: rec.FallbackReason}
	if log, err := os.ReadFile(rec.LogPath()); err == nil {
		res.Answer = string(log)
	}
	return NewEnvelope(res, runErr, rec.ID)
}

// AskTarget bundles the ask flag pointers that a logged dispatch's request overwrites on --job/--retry.
type AskTarget struct {
	Name, Provider, Model, Effort, Workdir, Session                   *string
	Tools, ReadOnly, NoTools, Autonomous, Fleet, EnableMemory, Direct *bool
	NoPersist, SafeMode                                               *bool
	DebugFile, SystemPrompt, AppendSystemPrompt, OutputSchema         *string
	Host, RemoteBin, Fallback                                         *string
	Timeout                                                           *time.Duration
	Persona                                                           **Schema
	Attach, KeepEnv                                                   *[]string
}

// Apply copies a logged dispatch's request onto this ask invocation's flags; the env-names note goes to errw.
func (t *AskTarget) Apply(rec Record, errw io.Writer) {
	*t.Name, *t.Provider, *t.Model, *t.Effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
	*t.Workdir, *t.Session, *t.Tools, *t.Timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
	*t.Attach, *t.Autonomous, *t.Fleet, *t.EnableMemory = rec.Request.Attach, rec.Request.Autonomous, rec.Request.Fleet, rec.Request.EnableMemory
	*t.KeepEnv, *t.Direct, *t.NoTools, *t.ReadOnly = rec.Request.KeepEnv, rec.Request.Direct, rec.Request.NoTools, rec.Request.ReadOnly
	*t.NoPersist, *t.SafeMode, *t.DebugFile = rec.Request.NoPersist, rec.Request.SafeMode, rec.Request.DebugFile
	*t.SystemPrompt, *t.AppendSystemPrompt, *t.OutputSchema = rec.Request.SystemPrompt, rec.Request.AppendSystemPrompt, rec.Request.OutputSchema
	*t.Host, *t.RemoteBin = rec.Host, rec.RemoteBin
	if t.Persona != nil {
		*t.Persona = rec.Request.Persona
	}
	if rec.FallbackPolicy != "" { // records from before the field keep this run's default
		*t.Fallback = rec.FallbackPolicy
	}
	if len(rec.Request.EnvNames) > 0 {
		fmt.Fprintf(errw, "ask: the logged run set env %s; values are not stored, so pass --env or --overlay again\n", strings.Join(rec.Request.EnvNames, ","))
	}
}

// ErrJobNotFresh marks a --job record that is not a fresh background job.
var ErrJobNotFresh = errors.New("not a fresh background job")

// LoadJobRecord loads a --job record for a background run continuation and claims it for this process.
func LoadJobRecord(id string) (Record, error) {
	r, err := LoadRecord(id)
	if err != nil {
		return r, err
	}
	if r.Status != "running" || r.PID != 0 {
		return r, fmt.Errorf("--job %s: record is %s (pid %d), %w", id, r.Status, r.PID, ErrJobNotFresh)
	}
	r.PID = os.Getpid()
	return r, nil
}

// ResolveOverlay checks the --env/--keep-env shorthand and resolves the file overlay's vault refs, failing closed; a
// --background parent leaves the file to its --job child, which inherits $COWORKER_OVERLAY and resolves it itself.
func ResolveOverlay(file agentconfig.Overlay, env, keep []string, background bool) (agentconfig.Resolved, error) {
	if _, err := agentconfig.FlagOverlay(env, keep); err != nil {
		return agentconfig.Resolved{}, err
	}
	if background {
		return agentconfig.Resolved{}, nil
	}
	return agentconfig.ResolveOverlay(file)
}

// OverlayLocal refuses a file overlay on --live or --host: neither carries it, and --host would expose it to ps.
func OverlayLocal(file agentconfig.Overlay, live bool, host string) error {
	if !file.Empty() && (live || host != "") {
		return errors.New("an overlay cannot reach --live or --host: run the agent locally")
	}
	return nil
}

// ParseAgentDefs parses --agents; --host cannot carry an inline definition.
func ParseAgentDefs(agents, host string) (map[string]*Schema, error) {
	var defs map[string]*Schema
	if agents != "" {
		var err error
		if defs, err = ParseAgents(agents); err != nil {
			return nil, err
		}
	}
	if host != "" && agents != "" {
		return nil, errors.New("--host cannot be combined with --agents: the remote cannot see an inline definition")
	}
	return defs, nil
}
