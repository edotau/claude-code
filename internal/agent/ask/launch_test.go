package ask

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
)

func TestWarnVersion(t *testing.T) {
	setup := func(version string, tty bool) (string, *bytes.Buffer) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		bin := pathstest.FakeVendor(t, "opencode", "echo 'opencode "+version+"'")
		var buf bytes.Buffer
		oldTTY, oldOut := agentVersionTTY, versionWarnOut
		agentVersionTTY, versionWarnOut = func() bool { return tty }, &buf
		t.Cleanup(func() { agentVersionTTY, versionWarnOut = oldTTY, oldOut })
		return bin, &buf
	}

	bin, buf := setup("2.0.0", true)
	warnVersion("opencode", bin, agent.LaunchOptions{}, nil)
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "opencode 2.0.0 above 1.x") {
		t.Errorf("interactive launch warning = %q, want exactly one line naming `opencode 2.0.0 above 1.x`", got)
	}

	for name, tc := range map[string]struct {
		args []string
		o    agent.LaunchOptions
		tty  bool
		ver  string
	}{
		"--version":    {[]string{"--version"}, agent.LaunchOptions{}, true, "2.0.0"},
		"run":          {[]string{"run", "x"}, agent.LaunchOptions{}, true, "2.0.0"},
		"login":        {[]string{"auth", "login"}, agent.LaunchOptions{}, true, "2.0.0"},
		"print-env":    {nil, agent.LaunchOptions{PrintEnv: true}, true, "2.0.0"},
		"non-terminal": {nil, agent.LaunchOptions{}, false, "2.0.0"},
		"in bounds":    {nil, agent.LaunchOptions{}, true, "1.5.0"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, buf := setup(tc.ver, tc.tty)
			warnVersion("opencode", bin, tc.o, tc.args)
			if buf.Len() != 0 {
				t.Errorf("warned %q, want silence", buf.String())
			}
		})
	}
}

// A models listing asked for under --profile launches behind that profile, so the agent's pin does not pick the route.
func TestModelsLaunchArgsCarryProfile(t *testing.T) {
	got := modelsLaunchArgs([]string{"models"}, "antigravity")
	if want := []string{"--profile", "antigravity", "models"}; !slices.Equal(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	opts, rest, err := agent.ParseLaunchArgs(got, false)
	if err != nil || opts.Provider != "antigravity" || !slices.Equal(rest, []string{"models"}) {
		t.Errorf("parsed profile=%q rest=%q err=%v", opts.Provider, rest, err)
	}
	if got := modelsLaunchArgs([]string{"models"}, ""); !slices.Equal(got, []string{"models"}) {
		t.Errorf("no profile: argv = %q, want the listing alone", got)
	}
}

// Every external vendor is a registered, external schema with its own engine; TestRosterFacts pins the rest.
func TestLookupKnownVendors(t *testing.T) {
	var engines []string
	for _, e := range agent.Engines() {
		engines = append(engines, e.Name())
	}
	for _, name := range []string{"codex", "gemini", "copilot", "opencode"} {
		s, ok := agent.LookupSchema(name)
		if !ok {
			t.Errorf("LookupSchema(%q) not found", name)
			continue
		}
		if s.Name != name || !s.External() {
			t.Errorf("LookupSchema(%q) = %q, external %v", name, s.Name, s.External())
		}
		if !slices.Contains(engines, name) {
			t.Errorf("schema %q has no engine", name)
		}
	}
}

// Every uncapped row names its transport (zero value is capped argv); gemini's ceiling sits below stdin.
func TestRosterPromptTransport(t *testing.T) {
	want := map[string]agent.PromptTransport{"claude": agent.PromptStdin, "codex": agent.PromptStdin,
		"copilot": agent.PromptMessage, "opencode": agent.PromptArgv, "gemini": agent.PromptStdin}
	for name, via := range want {
		s, ok := agent.LookupSchema(name)
		if !ok || s.CLI.PromptVia != via {
			t.Errorf("%s PromptVia = %v (found %v), want %v", name, s.CLI.PromptVia, ok, via)
		}
	}
	if agent.PromptArgv.MaxBytes() != agent.MaxPromptBytes || agent.PromptStdin.MaxBytes() != 0 {
		t.Errorf("MaxBytes: argv %d stdin %d", agent.PromptArgv.MaxBytes(), agent.PromptStdin.MaxBytes())
	}
	if g, _ := agent.LookupSchema("gemini"); g.CLI.PromptMaxBytes != 100*1024 {
		t.Errorf("gemini PromptMaxBytes = %d, want %d", g.CLI.PromptMaxBytes, 100*1024)
	}
}

func TestRosterMatchesSpecs(t *testing.T) {
	var names, runners []string
	for _, s := range agent.Roster() {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"claude", "codex", "copilot", "gemini", "opencode"}) {
		t.Fatalf("roster order = %v", names)
	}
	for _, r := range agent.Engines() {
		runners = append(runners, r.Name())
	}
	for label, other := range map[string][]string{
		"Engines": runners, "RouteAgents": slices.Clone(providers.RouteAgents),
	} {
		slices.Sort(other)
		want := slices.Clone(names)
		slices.Sort(want)
		if !slices.Equal(other, want) {
			t.Errorf("%s names = %v, roster names = %v", label, other, want)
		}
	}
}

func TestRosterFacts(t *testing.T) {
	catalogs := map[string]agent.CLI{
		"codex":    {ResumeFormat: "codex resume session", ModelsProvider: "openai"},
		"gemini":   {ResumeFormat: "agy --conversation session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"opencode": {ResumeFormat: "opencode run --session session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"copilot":  {ResumeFormat: "copilot --resume session", ModelsProvider: "github", ReadNeedsTools: true},
	}
	for _, s := range agent.Roster() {
		t.Run(s.Name, func(t *testing.T) {
			got, ok := agent.LookupSchema(s.Name)
			if !ok || !reflect.DeepEqual(got, s) {
				t.Fatalf("LookupSchema(%q) = %+v, %v; want %+v", s.Name, got, ok, s)
			}
			if s.External() != (s.Name != "claude") {
				t.Errorf("External() = %v", s.External())
			}
			assertRosterWireFacts(t, s, catalogs[s.Name])
		})
	}
	if s, ok := agent.LookupSchema("unknown"); ok || !reflect.DeepEqual(s, agent.Schema{}) {
		t.Errorf("unknown schema = %+v, %v", s, ok)
	}
	if (agent.Schema{}).External() {
		t.Error("zero schema is external")
	}
}

func assertRosterWireFacts(t *testing.T, s agent.Schema, want agent.CLI) {
	t.Helper()
	if s.Name == "claude" {
		if s.CLI.Binary != "" || s.CLI.Dialects != nil || s.CLI.RouterSuffix != nil {
			t.Errorf("claude wire facts = %+v", s.CLI)
		}
		return
	}
	if s.ResumeHint("session") != want.ResumeFormat {
		t.Error("resume command changed")
	}
	if !slices.Equal(s.CLI.ModelsArgv, want.ModelsArgv) || s.CLI.ModelsProvider != want.ModelsProvider ||
		s.CLI.ReadNeedsTools != want.ReadNeedsTools || !slices.Equal(s.CLI.ExtraUsage, want.ExtraUsage) {
		t.Error("catalog, tools or help facts changed")
	}
}

// Given a fact stated in both agentid.Routing and the roster row, Then the two values agree for every agent.
func TestRosterAgreesWithRouteFacts(t *testing.T) {
	for _, name := range agentid.All() {
		t.Run(name, func(t *testing.T) {
			s, ok := agent.LookupSchema(name)
			if !ok {
				t.Fatalf("%s has no roster row", name)
			}
			if name != agentid.Claude && s.CLI.Binary != agentid.Binary(name) {
				t.Errorf("binary: row %q, agentid %q", s.CLI.Binary, agentid.Binary(name))
			}
			if got := slices.Contains(s.CLI.Dialects, providers.ProtocolResponses); got != agentid.Routing(name).ResponsesRoute {
				t.Errorf("responses: row dialects %v, ResponsesRoute %v", s.CLI.Dialects, agentid.Routing(name).ResponsesRoute)
			}
			if s.CLI.Hub != agentid.Routing(name).SubLadder {
				t.Errorf("hub: row %v, SubLadder %v", s.CLI.Hub, agentid.Routing(name).SubLadder)
			}
		})
	}
}
