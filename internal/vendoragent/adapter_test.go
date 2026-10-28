package vendoragent

import (
	"slices"
	"testing"
)

// TestRoster pins the roster and its order, matching the verb table in contract.md.
func TestRoster(t *testing.T) {
	var got []string
	for _, a := range adapters {
		got = append(got, a.Name)
	}
	if !slices.Equal(got, []string{"codex", "gemini", "copilot", "opencode"}) {
		t.Errorf("roster = %v, want [codex gemini copilot opencode]", got)
	}
}

// TestLookupKnownVendors: each of the four vendors resolves with the ResumeHint and model-catalog
// wiring frozen in contract.md's resume-commands line and Adapter's ModelsArgv/ModelsProvider.
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
		a, ok := Lookup(c.name)
		if !ok {
			t.Errorf("Lookup(%q) not found", c.name)
			continue
		}
		if a.Name != c.name {
			t.Errorf("Lookup(%q).Name = %q", c.name, a.Name)
		}
		if a.Binary == "" {
			t.Errorf("Lookup(%q).Binary is empty, want a fallback at least", c.name)
		}
		if a.ResumeHint == nil {
			t.Errorf("Lookup(%q).ResumeHint is nil", c.name)
		} else if got := a.ResumeHint(c.resumeID); got != c.wantResumeHint {
			t.Errorf("Lookup(%q).ResumeHint(%q) = %q, want %q", c.name, c.resumeID, got, c.wantResumeHint)
		}
		if a.ModelsProvider != c.wantModelsProv {
			t.Errorf("Lookup(%q).ModelsProvider = %q, want %q", c.name, a.ModelsProvider, c.wantModelsProv)
		}
		if !slices.Equal(a.ModelsArgv, c.wantModelsArgv) {
			t.Errorf("Lookup(%q).ModelsArgv = %v, want %v", c.name, a.ModelsArgv, c.wantModelsArgv)
		}
		if a.Login == nil {
			t.Errorf("Lookup(%q).Login is nil", c.name)
		}
		if a.ReadNeedsTools != c.wantReadNeedsTools {
			t.Errorf("Lookup(%q).ReadNeedsTools = %v, want %v", c.name, a.ReadNeedsTools, c.wantReadNeedsTools)
		}
	}
}

// TestLookupUnknownVendor: an unregistered name reports not-found, not a zero-value adapter mistaken for real.
func TestLookupUnknownVendor(t *testing.T) {
	if a, ok := Lookup("nonexistent-vendor"); ok || a.Name != "" {
		t.Errorf("Lookup(unknown) = (%+v, %v), want (Adapter{}, false)", a, ok)
	}
}
