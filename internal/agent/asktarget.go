package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
)

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
