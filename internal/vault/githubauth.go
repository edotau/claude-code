package vault

import (
	"fmt"
	"os"
	"strings"
)

// gitHubAuthKey is the Overlay entry holding the GitHub auth profile (gh's active account); "" follows gh.
const gitHubAuthKey = "github-auth"

// GitHubAuth is the stored GitHub auth profile name ("" when unset or unreadable).
func GitHubAuth() string {
	var name string
	err := View(func(tx *Tx) error {
		name, _ = tx.Get(Overlay, gitHubAuthKey)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault: cannot read %s (following gh active): %v\n", gitHubAuthKey, err)
	}
	return strings.TrimSpace(name)
}

// SetGitHubAuth stores the GitHub auth profile name; "" removes it.
func SetGitHubAuth(profile string) error {
	return Update(func(tx *Tx) error { return tx.Put(Overlay, gitHubAuthKey, strings.TrimSpace(profile)) })
}
