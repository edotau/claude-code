package cli

import (
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCmdConfig(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"nil", nil, 0},
		{"short help", []string{"-h"}, 0},
		{"long help", []string{"--help"}, 0},
		{"unknown", []string{"nope"}, 2},
		{"show extra arg", []string{"show", "x"}, 2},
		{"edit extra arg", []string{"edit", "x"}, 2},
		{"show", []string{"show"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			if got := cmdConfig(tt.args); got != tt.want {
				t.Errorf("cmdConfig(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestConfigVerbsAndMenu(t *testing.T) {
	if len(configVerbs) != 2 {
		t.Errorf("configVerbs has %d entries, want exactly show and edit", len(configVerbs))
	}
	for _, name := range []string{"show", "edit"} {
		t.Run(name, func(t *testing.T) {
			if configVerbs[name] == nil {
				t.Errorf("configVerbs[%q] missing or nil", name)
			}
		})
	}
	count := 0
	for _, c := range commands() {
		if c.name != "config" {
			continue
		}
		count++
		if c.group != groupSettings {
			t.Errorf("config group = %q, want %q", c.group, groupSettings)
		}
		if n := utf8.RuneCountInString(c.summary); n > summaryMax {
			t.Errorf("config summary has %d characters, max %d", n, summaryMax)
		}
		if c.hidden || c.run == nil {
			t.Error("config menu row is hidden or has no handler")
		}
	}
	if count != 1 {
		t.Errorf("commands() has %d config rows, want 1", count)
	}
}
