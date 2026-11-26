package rtkwrap

import "testing"

func TestVetoRuleLsAll(t *testing.T) {
	for cmd, want := range map[string]string{
		"ls -la d":             "ls-all",
		"ls -A":                "ls-all",
		"ls --all d":           "ls-all",
		"ls --almost-all":      "ls-all",
		"cd x && ls -a":        "ls-all",
		"ls -l d":              "",
		"git branch -D b":      "branch-mutate",
		"git branch -a":        "",
		"git -C d branch":      "",
		"git -c a=b branch -a": "",
		"ls":                   "",
		"git status -sz":       "machine-flag", // a cluster holding -z
		"git status -bz":       "machine-flag",
	} {
		if got := VetoRule(cmd, ""); got != want {
			t.Errorf("VetoRule(%q) = %q, want %q", cmd, got, want)
		}
	}
}
