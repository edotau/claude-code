package agent

import (
	"context"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Check is one `claude-code <v> setup` line.
type Check struct {
	Name, State, Detail string // State: ok | fail | warn
}

// Report is the full setup result for one agent.
type Report struct {
	Agent  string
	Checks []Check
	OK     bool // binary ok && login ok
}

// Setup runs the binary, login, route and projection checks for an agent. Text rendering is the caller's job.
func (s Schema) Setup() Report {
	if !s.External() {
		return Report{Agent: s.Name, Checks: []Check{{Name: "agent", State: "fail", Detail: errUnknownAgent(s.Name).Error()}}}
	}
	binCheck := binaryCheck(s)
	loginCheck := loginCheck(s)
	rep := Report{
		Agent:  s.Name,
		Checks: []Check{binCheck, loginCheck, s.routeCheck(), s.projectionCheck()},
		OK:     binCheck.State == "ok" && loginCheck.State == "ok",
	}
	return rep
}

// binaryCheck looks up the agent executable via paths.AgentBin.
func binaryCheck(s Schema) Check {
	p, err := paths.AgentBin(s.Name)
	if err != nil {
		return Check{Name: "binary", State: "fail", Detail: err.Error()}
	}
	return Check{Name: "binary", State: "ok", Detail: p}
}

// loginCheck runs the CLI's own login probe.
func loginCheck(s Schema) Check {
	if !s.LoginPresent() {
		return Check{Name: "login", State: "fail", Detail: "no " + s.Name + " login found (claude-code vault set / agent's own login)"}
	}
	return Check{Name: "login", State: "ok", Detail: s.Name + " login present"}
}

// routeCheck pulls the "<name> route" detail from RouteDetail, without running the full doctor sweep.
func (s Schema) routeCheck() Check {
	detail, ok := s.eng().RouteDetail()
	if !ok {
		return Check{Name: "route", State: "warn", Detail: "no route check found for " + s.Name}
	}
	return Check{Name: "route", State: "ok", Detail: detail}
}

// projectionCheck reports whether state/projections/<agent>/agents has any rendered agents.
func (s Schema) projectionCheck() Check {
	dir := project.Dir(paths.ConfigDir(), s.Name)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return Check{Name: "projection", State: "warn", Detail: fmt.Sprintf("no rendered agents at %s (claude-code agents sync)", dir)}
	}
	return Check{Name: "projection", State: "ok", Detail: fmt.Sprintf("%d agent(s) at %s", len(entries), dir)}
}

// the optional Authenticator a Driver adds, and the agent-level entry points over it.

// Authenticator is the optional sign-in surface a Driver adds; BaseEngine does not implement it.
type Authenticator interface {
	// AuthStatus is the CLI's own sign-in from local state (files, keyring, a local status command); no network.
	AuthStatus() providers.AuthState
	// Plan is the subscription st's sign-in holds; it may reach the network and caches per account itself.
	Plan(ctx context.Context, st providers.AuthState) providers.Subscription
	// Verify probes the sign-in for the provider sign-in rows; a failed probe is VerdictTransient.
	Verify(ctx context.Context) (providers.AuthVerdict, string)
}

func authenticator(name string) (Authenticator, bool) {
	a, ok := schemaFor(name).eng().(Authenticator)
	return a, ok
}

// AuthStatus is name's own sign-in; the zero state for an agent with no Authenticator. Safe for concurrent use.
func AuthStatus(name string) providers.AuthState {
	if a, ok := authenticator(name); ok {
		return a.AuthStatus()
	}
	return providers.AuthState{}
}

// AuthPlan is name's subscription for st; zero when st is not present or the agent has no Authenticator.
func AuthPlan(ctx context.Context, name string, st providers.AuthState) providers.Subscription {
	if a, ok := authenticator(name); ok && st.Present {
		return a.Plan(ctx, st)
	}
	return providers.Subscription{}
}

// Verify probes name's own CLI sign-in for the provider sign-in rows.
func Verify(ctx context.Context, name string) (providers.AuthVerdict, string) {
	if a, ok := authenticator(name); ok {
		return a.Verify(ctx)
	}
	return providers.VerdictTransient, "unknown sign-in " + name
}

// Configured is the no-network presence check: the agent binary, plus an opencode login for the cell's provider.
func Configured(agent string, c router.Cell) error {
	s, ok := LookupSchema(agent)
	if !ok {
		return fmt.Errorf("unknown agent %s", agent)
	}
	if !s.External() {
		return nil
	}
	if _, err := paths.AgentBin(s.CLI.Binary); err != nil {
		return err
	}
	return s.eng().Presence(c)
}
