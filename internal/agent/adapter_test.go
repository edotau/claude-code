package agent

import (
	"slices"
	"testing"
)

func TestRoster(t *testing.T) {
	if got := Names(); !slices.Equal(got, []string{"codex", "gemini", "opencode", "copilot"}) {
		t.Errorf("external roster = %v, want [codex gemini opencode copilot]", got)
	}
}

// TestLookupKnownVendors pins resume commands, model catalogs and file-read permissions.
func TestLookupKnownVendors(t *testing.T) {
	cases := []struct {
		name               string
		resumeID           string
		wantResumeHint     string
		wantModelsProv     string
		wantModelsArgv     []string
		wantReadNeedsTools bool
	}{
		{"codex", "abc123", "codex resume abc123", "openai", nil, false},
		{"gemini", "abc123", "agy --conversation abc123", "", []string{"models"}, true},
		{"copilot", "abc123", "copilot --resume abc123", "github", nil, true},
		{"opencode", "abc123", "opencode run --session abc123", "", []string{"models"}, true},
	}
	for _, c := range cases {
		s, ok := LookupSchema(c.name)
		if !ok {
			t.Errorf("LookupSchema(%q) not found", c.name)
			continue
		}
		if s.Name != c.name {
			t.Errorf("LookupSchema(%q).Name = %q", c.name, s.Name)
		}
		if !s.External() {
			t.Errorf("LookupSchema(%q) is not external", c.name)
		}
		if got := s.ResumeHint(c.resumeID); got != c.wantResumeHint {
			t.Errorf("schema %q resume = %q, want %q", c.name, got, c.wantResumeHint)
		}
		if s.CLI.ModelsProvider != c.wantModelsProv {
			t.Errorf("schema %q ModelsProvider = %q, want %q", c.name, s.CLI.ModelsProvider, c.wantModelsProv)
		}
		if !slices.Equal(s.CLI.ModelsArgv, c.wantModelsArgv) {
			t.Errorf("schema %q ModelsArgv = %v, want %v", c.name, s.CLI.ModelsArgv, c.wantModelsArgv)
		}
		if _, ok := engines[s.Name]; !ok {
			t.Errorf("schema %q has no engine", c.name)
		}
		if s.CLI.ReadNeedsTools != c.wantReadNeedsTools {
			t.Errorf("schema %q ReadNeedsTools = %v, want %v", c.name, s.CLI.ReadNeedsTools, c.wantReadNeedsTools)
		}
	}
}

func TestLookupUnknownVendor(t *testing.T) {
	if s, ok := LookupSchema("nonexistent-vendor"); ok || s.Name != "" {
		t.Errorf("unknown schema = (%+v, %v), want (Schema{}, false)", s, ok)
	}
}

func TestKnownIncludesClaude(t *testing.T) {
	for _, s := range Roster() {
		if _, ok := LookupSchema(s.Name); !ok {
			t.Errorf("LookupSchema(%q) = false", s.Name)
		}
	}
	if _, ok := LookupSchema("nonexistent-vendor"); ok {
		t.Error("unknown agent is known")
	}
}

func TestSetupRejectsNonExternal(t *testing.T) {
	for _, name := range []string{"claude", "nonexistent-vendor"} {
		rep := schemaFor(name).Setup()
		if rep.OK || len(rep.Checks) != 1 || rep.Checks[0].State != "fail" ||
			rep.Checks[0].Detail != errUnknownAgent(name).Error() {
			t.Errorf("Setup(%q) = %+v", name, rep)
		}
	}
}
