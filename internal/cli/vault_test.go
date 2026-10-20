package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCmdVault(t *testing.T) {
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
		{"migrate nothing", []string{"migrate"}, 0},
		{"migrate dry-run nothing", []string{"migrate", "--dry-run"}, 0},
		{"migrate extra arg", []string{"migrate", "x"}, 2},
		{"migrate bad flag", []string{"migrate", "--nope"}, 2},
		{"finalize no manifest", []string{"finalize"}, 1},
		{"finalize extra arg", []string{"finalize", "x"}, 2},
		{"rollback no manifest", []string{"rollback"}, 1},
		{"rollback extra arg", []string{"rollback", "x"}, 2},
		{"rotate-key extra arg", []string{"rotate-key", "x"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.Use(t)
			if got := cmdVault(tt.args); got != tt.want {
				t.Errorf("cmdVault(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestVaultVerbsAndMenu(t *testing.T) {
	want := []string{"show", "edit", "migrate", "finalize", "rollback", "rotate-key"}
	if len(vaultVerbs) != len(want) {
		t.Errorf("vaultVerbs has %d entries, want exactly %v", len(vaultVerbs), want)
	}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			if vaultVerbs[name] == nil {
				t.Errorf("vaultVerbs[%q] missing or nil", name)
			}
		})
	}
	count := 0
	for _, c := range commands() {
		if c.name != "vault" {
			continue
		}
		count++
		if c.group != groupAuth {
			t.Errorf("vault group = %q, want %q", c.group, groupAuth)
		}
		if n := utf8.RuneCountInString(c.summary); n > summaryMax {
			t.Errorf("vault summary has %d characters, max %d", n, summaryMax)
		}
		if c.hidden || c.run == nil {
			t.Error("vault menu row is hidden or has no handler")
		}
	}
	if count != 1 {
		t.Errorf("commands() has %d vault rows, want 1", count)
	}
}

func TestConfigVerbIsUnknown(t *testing.T) {
	vaulttest.Use(t)
	code, _, stderr := captureOutput(t, func() int { return Run([]string{"claude-code", "config", "show"}) })
	if code != 2 {
		t.Errorf("Run([config show]) = %d, want 2 (unknown verb)", code)
	}
	if !strings.Contains(stderr, `unknown command "config"`) {
		t.Errorf("stderr = %q, want it to name config as unknown", stderr)
	}
}

func TestVaultInAuthGroupHelp(t *testing.T) {
	rows := menuRows()
	group := ""
	found := false
	for _, r := range rows {
		if r.head != "" {
			group = r.head
			continue
		}
		if strings.TrimSpace(r.name) == "vault" {
			found = true
			if group != groupAuth {
				t.Errorf("vault appears under menu group %q, want %q", group, groupAuth)
			}
		}
	}
	if !found {
		t.Fatal("menuRows() has no vault row")
	}
}
