package shell

import (
	"reflect"
	"testing"
)

// TestAliasVerbCaseInsensitive pins that git's case-insensitive alias names expand to the real verb.
func TestAliasVerbCaseInsensitive(t *testing.T) {
	if got := collectGit("git -c alias.xx=reset XX --hard", "/w"); !reflect.DeepEqual(got, []gitCall{{"/w", "reset"}}) {
		t.Errorf("alias XX = %v, want reset", got)
	}
	if !GitCommitInvocation("git -c alias.ci=commit CI --allow-empty -m x") {
		t.Error("upper-case alias of commit not seen as a commit")
	}
}
