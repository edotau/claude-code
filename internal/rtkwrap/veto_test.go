package rtkwrap

import "testing"

func TestVetoRuleLsAll(t *testing.T) {
	for cmd, want := range map[string]string{
		"ls -la d":        "ls-all",
		"ls -A":           "ls-all",
		"ls --all d":      "ls-all",
		"ls --almost-all": "ls-all",
		"cd x && ls -a":   "ls-all",
		"ls -l d":         "",
		"ls":              "",
	} {
		if got := VetoRule(cmd, ""); got != want {
			t.Errorf("VetoRule(%q) = %q, want %q", cmd, got, want)
		}
	}
}
