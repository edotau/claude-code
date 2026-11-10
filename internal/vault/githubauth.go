package vault

import (
	"fmt"
	"os"
	"strings"
)

// GitHubAuthKey is the Overlay entry holding the GitHub auth profile (gh's active account); "" follows gh.
const GitHubAuthKey = "github-auth"

// GitHubAuth is the stored GitHub auth profile name ("" when unset or unreadable).
func GitHubAuth() string {
	var name string
	err := View(func(tx *Tx) error {
		name, _ = tx.Get(Overlay, GitHubAuthKey)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault: cannot read %s (following gh active): %v\n", GitHubAuthKey, err)
	}
	return strings.TrimSpace(name)
}

// SetGitHubAuth stores the GitHub auth profile name; "" removes it.
func SetGitHubAuth(profile string) error {
	return Update(func(tx *Tx) error { return tx.Put(Overlay, GitHubAuthKey, strings.TrimSpace(profile)) })
}
