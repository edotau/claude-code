package vaultdoc

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// EnvChange is one vault env write: Old is "" when the name was unset, New is "" for a removal.
type EnvChange struct{ Name, Old, New string }

// SetEnv writes name=value in the vault env bucket through the menu's writer; dryRun writes nothing.
func SetEnv(name, value string, dryRun bool) (EnvChange, error) {
	value = strings.TrimSpace(value)
	if err := checkEnvSet(name, value); err != nil {
		return EnvChange{}, err
	}
	ch := EnvChange{Name: name, Old: providers.CustomEnv()[name], New: value}
	if _, ref := providers.SecretRefName(name, ch.Old); ref {
		return ch, fmt.Errorf("%s is an %s reference; change it in `claude-code configure` → environment", name, providers.SecretRef)
	}
	if dryRun || ch.Old == value {
		return ch, nil
	}
	return ch, configure.SaveEnvEdit(name, value, "")
}

// checkEnvName is the menu's name check for a new environment variable.
func checkEnvName(name string) error {
	if !vault.EnvName.MatchString(name) {
		return fmt.Errorf("%q is not a valid name (letters, digits, underscore; not starting with a digit)", name)
	}
	return nil
}

// checkEnvSet applies the menu's name check, then refuses what belongs to the sealed-secret path: names the
// vault or the harness owns or that look like credentials, and values `doctor --scan` flags in this bucket.
func checkEnvSet(name, value string) error {
	if err := checkEnvName(name); err != nil {
		return err
	}
	switch {
	case value == "":
		return fmt.Errorf("%s: empty value; `vault env unset %s` removes it", name, name)
	case secretEnvName(name):
		return fmt.Errorf("%s is a secret name; the env bucket is not sealed, use `claude-code vault set %s`", name, name)
	case providers.LooksLikeSecret(value):
		return fmt.Errorf("%s: the value looks like a credential; the env bucket is not sealed, "+
			"use `claude-code vault set %s`", name, name)
	}
	if _, ref := providers.SecretRefName(name, value); ref {
		return fmt.Errorf("%s: an %s value seals a secret; add it in `claude-code configure` → environment",
			name, providers.SecretRef)
	}
	return nil
}

// secretEnvName reports a name the sealed-secret path owns: harness-internal, a route credential, a stored secret
// or a credential-shaped name (KEY, TOKEN, AUTH, ...).
func secretEnvName(name string) bool {
	return providers.InternalSecretName(name) || providers.IsSecretName(strings.ToUpper(name)) ||
		providers.RouteSecretGuard()(name) || slices.Contains(providers.SecretNames(), name)
}

// UnsetEnv removes name from the vault env bucket through the menu's writer; dryRun writes nothing.
func UnsetEnv(name string, dryRun bool) (EnvChange, error) {
	if err := checkEnvName(name); err != nil {
		return EnvChange{}, err
	}
	ch := EnvChange{Name: name, Old: providers.CustomEnv()[name]}
	if dryRun || ch.Old == "" {
		return ch, nil
	}
	return ch, configure.SaveEnvEdit(name, "", "")
}
