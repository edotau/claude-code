package agent

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
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
	detail, ok := s.Eng().RouteDetail()
	if !ok {
		return Check{Name: "route", State: "warn", Detail: "no route check found for " + s.Name}
	}
	return Check{Name: "route", State: s.routeState(), Detail: detail}
}

// routeState warns when copilot's dispatch is not the route it is pinned to: no route, or a fallback off the pin.
func (s Schema) routeState() string {
	if s.Name != agentid.Copilot {
		return "ok"
	}
	reg, err := providers.Load()
	if err != nil {
		return "warn"
	}
	ri := s.DescribeRoute(reg)
	if !ri.OK || ri.Route != RouteCopilotLogin && ri.Provider != vault.GetAgentRecord(s.Name).Profile {
		return "warn"
	}
	return "ok"
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

// configured is the no-network presence check: the agent binary, plus an opencode login for the cell's provider.
func configured(agent string, c router.Cell) error {
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
	return s.Eng().Presence(c)
}
