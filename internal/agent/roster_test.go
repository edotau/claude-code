package agent

import (
	"reflect"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestRosterMatchesSpecs(t *testing.T) {
	var names, runners []string
	for _, s := range Roster() {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"claude", "codex", "gemini", "opencode", "copilot"}) {
		t.Fatalf("roster order = %v", names)
	}
	for _, r := range Runners() {
		runners = append(runners, r.Name())
	}
	for label, other := range map[string][]string{
		"Runners": runners, "RouteRunners": slices.Clone(providers.RouteRunners),
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
	catalogs := map[string]CLI{
		"codex": {ResumeFormat: "codex resume session", ModelsProvider: "openai"},
		"gemini": {ResumeFormat: "agy --conversation session", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
			ExtraUsage: [][2]string{{"bridge", "ingest files for a large-context task"}}},
		"opencode": {ResumeFormat: "opencode run --session session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"copilot":  {ResumeFormat: "copilot --resume session", ModelsProvider: "github", ReadNeedsTools: true},
	}
	for _, s := range Roster() {
		t.Run(s.Name, func(t *testing.T) {
			got, ok := LookupSchema(s.Name)
			if !ok || !reflect.DeepEqual(got, s) {
				t.Fatalf("LookupSchema(%q) = %+v, %v; want %+v", s.Name, got, ok, s)
			}
			if s.External() != (s.Name != "claude") {
				t.Errorf("External() = %v", s.External())
			}
			if s.Name == "claude" {
				if s.CLI.Binary != "" || s.CLI.Dialects != nil || s.CLI.RouterSuffix != nil {
					t.Errorf("claude wire facts = %+v", s.CLI)
				}
				return
			}
			want := catalogs[s.Name]
			if s.ResumeHint("session") != want.ResumeFormat {
				t.Error("resume command changed")
			}
			if !slices.Equal(s.CLI.ModelsArgv, want.ModelsArgv) || s.CLI.ModelsProvider != want.ModelsProvider ||
				s.CLI.ReadNeedsTools != want.ReadNeedsTools || !slices.Equal(s.CLI.ExtraUsage, want.ExtraUsage) {
				t.Error("catalog, tools or help facts changed")
			}
		})
	}
	if s, ok := LookupSchema("unknown"); ok || !reflect.DeepEqual(s, Schema{}) {
		t.Errorf("unknown schema = %+v, %v", s, ok)
	}
	if (Schema{}).External() {
		t.Error("zero schema is external")
	}
}

func TestSchemaResumeHintEmpty(t *testing.T) {
	for _, s := range Roster() {
		if got := s.ResumeHint(""); got != "" {
			t.Errorf("schema %q empty session resume = %q", s.Name, got)
		}
		if !s.External() && s.ResumeHint("session") != "" {
			t.Errorf("schema %q without a resume format rendered a command", s.Name)
		}
	}
	if got := (Schema{}).ResumeHint("session"); got != "" {
		t.Errorf("zero schema resume = %q", got)
	}
}

func TestRosterReturnsCopies(t *testing.T) {
	want := Roster()
	rows := Roster()
	for i := range rows {
		s := &rows[i]
		s.Name = "changed"
		s.CLI.Caps.Model = None
		if len(s.Tools) > 0 {
			s.Tools[0] = "changed"
		}
		for _, values := range [][]string{s.CLI.Dialects, s.CLI.Events, s.CLI.ModelsArgv, s.CLI.LoginArgv} {
			if len(values) > 0 {
				values[0] = "changed"
			}
		}
		if len(s.CLI.ExtraUsage) > 0 {
			s.CLI.ExtraUsage[0][0] = "changed"
		}
		if s.CLI.RouterSuffix != nil {
			s.CLI.RouterSuffix["changed"] = "changed"
		}
	}
	if got := Roster(); !reflect.DeepEqual(got, want) {
		t.Error("mutating a roster changed later calls")
	}
	s, _ := LookupSchema("opencode")
	s.CLI.Events[0] = "changed"
	s.CLI.ModelsArgv[0] = "changed"
	s.CLI.LoginArgv[0] = "changed"
	s.CLI.Dialects[0] = "changed"
	s.CLI.RouterSuffix["changed"] = "changed"
	if got := Roster(); !reflect.DeepEqual(got, want) {
		t.Error("mutating a lookup changed later calls")
	}
}
