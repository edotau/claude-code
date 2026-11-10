package shell

import (
	"testing"
)

func TestReadOnlyGitLeadingNoPager(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git --no-pager status":                           true,
		"git --no-pager status --short --branch":          true,
		"git --no-pager diff --no-ext-diff --no-textconv": true,
		"git --no-pager push":                             false,
		"git --no-pager --no-pager status":                false,
		"git --no-pager=x status":                         false,
		"git -c core.pager=x status":                      false,
		"git --config-env=a=b status":                     false,
		"git --paginate status":                           false,
		"git --no-pager":                                  false,
	} {
		if got := ReadOnlyGit(cmd); got != want {
			t.Errorf("ReadOnlyGit(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestReadOnlyGitRefusesOutsidePaths(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git --no-pager diff --no-ext-diff --no-textconv /etc/hostname /etc/hosts":  false,
		"git --no-pager diff --no-ext-diff --no-textconv ../a ../b":                 false,
		"git --no-pager diff --no-ext-diff --no-textconv -- /etc/hostname":          false,
		"git --no-pager diff --no-ext-diff --no-textconv internal/shell/scope.go":   true,
		"git --no-pager log --no-ext-diff --no-textconv -- internal/shell/scope.go": true,
	} {
		if got := ReadOnlyGit(cmd); got != want {
			t.Errorf("ReadOnlyGit(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// TestReadOnlyGitRefusesNoIndex pins that --no-index (reads outside the repo) is refused for diff and grep.
func TestReadOnlyGitRefusesNoIndex(t *testing.T) {
	for _, cmd := range []string{
		"git diff --no-ext-diff --no-textconv --no-index /etc/passwd /dev/null",
		"git grep --no-index -e secret /etc",
	} {
		if ReadOnlyGit(cmd) {
			t.Errorf("ReadOnlyGit(%q) = true", cmd)
		}
	}
}
