package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
)

// Allowed values for the enum fields of AgentSettings.
const (
	FallbackNone   = "none"
	FallbackPin    = "pin"
	FallbackAgents = "agents"
	FallbackAll    = "all"

	RunnerPi  = "pi"
	RunnerOwn = "own"

	CodemodeOff  = "off"
	CodemodeOn   = "on"
	CodemodeOnly = "only"

	VerbosityLow    = "low"
	VerbosityMedium = "medium"
	VerbosityHigh   = "high"

	// ConfigRouteSession is the config_route value meaning the session's own route.
	ConfigRouteSession = "session"
)

// FallbackModes, Runners, Codemodes and Verbosities list the allowed values of their fields.
var (
	FallbackModes = []string{FallbackNone, FallbackPin, FallbackAgents, FallbackAll}
	Runners       = []string{RunnerPi, RunnerOwn}
	Codemodes     = []string{CodemodeOff, CodemodeOn, CodemodeOnly}
	Verbosities   = []string{VerbosityLow, VerbosityMedium, VerbosityHigh}
)

// Budget caps one run; zero means unset.
type Budget struct {
	USD           float64 `json:"usd,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	MaxChildren   int     `json:"max_children,omitempty"`
	MaxTreeTokens int     `json:"max_tree_tokens,omitempty"`
}

// ConfigRoute is either the session's route (Session) or an explicit Route.
type ConfigRoute struct {
	Session bool
	Route   Route
}

// MarshalJSON writes the string "session" or the route object.
func (c ConfigRoute) MarshalJSON() ([]byte, error) {
	if c.Session {
		return json.Marshal(ConfigRouteSession)
	}
	return json.Marshal(c.Route)
}

// UnmarshalJSON reads the string "session" or a route object.
func (c *ConfigRoute) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != ConfigRouteSession {
			return fmt.Errorf("config_route: %q is not %q or a route", s, ConfigRouteSession)
		}
		*c = ConfigRoute{Session: true}
		return nil
	}
	var r Route
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("config_route: %w", err)
	}
	*c = ConfigRoute{Route: r}
	return nil
}

// AgentSettings is the one shape of an agent's settings, shared by every surface that spells them.
type AgentSettings struct {
	Route         *Route            `json:"route,omitempty"`
	FallbackRoute *Route            `json:"fallback_route,omitempty"`
	FallbackMode  string            `json:"fallback_mode,omitempty"`
	Model         string            `json:"model,omitempty"`
	Effort        string            `json:"effort,omitempty"`
	Runner        string            `json:"runner,omitempty"`
	Codemode      string            `json:"codemode,omitempty"`
	Tools         *bool             `json:"tools,omitempty"`
	Timeout       string            `json:"timeout,omitempty"`
	Verbosity     string            `json:"verbosity,omitempty"`
	ConfigRoute   *ConfigRoute      `json:"config_route,omitempty"`
	Context       string            `json:"context,omitempty"`
	Budget        *Budget           `json:"budget,omitempty"`
	Extras        map[string]string `json:"extras,omitempty"`
}

// Validate reports the first field outside its allowed values; empty fields are valid.
func (s AgentSettings) Validate() error {
	for _, c := range []struct {
		field, v string
		allowed  []string
	}{
		{"fallback_mode", s.FallbackMode, FallbackModes},
		{"effort", s.Effort, agentid.EffortNames()},
		{"runner", s.Runner, Runners},
		{"codemode", s.Codemode, Codemodes},
		{"verbosity", s.Verbosity, Verbosities},
	} {
		if c.v != "" && !slices.Contains(c.allowed, c.v) {
			return fmt.Errorf("%s: %q is not one of %s", c.field, c.v, strings.Join(c.allowed, "|"))
		}
	}
	return s.validateNumbers()
}

func (s AgentSettings) validateNumbers() error {
	if s.Timeout != "" {
		if _, err := time.ParseDuration(s.Timeout); err != nil {
			return fmt.Errorf("timeout: %q is not a duration like 90s or 10m", s.Timeout)
		}
	}
	if b := s.Budget; b != nil && (b.USD < 0 || b.Credits < 0 || b.MaxChildren < 0 || b.MaxTreeTokens < 0) {
		return errors.New("budget: usd, credits, max_children and max_tree_tokens must be >= 0")
	}
	return nil
}
