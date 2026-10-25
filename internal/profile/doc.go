// Package profile edits the per-agent profile table and migrates the Databricks provider twins to presets.
package profile

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// CheckName rejects a set name that is not a plain word.
func CheckName(n string) error {
	if !nameRE.MatchString(n) {
		return fmt.Errorf("profile name %q must match [A-Za-z0-9][A-Za-z0-9._-]*", n)
	}
	return nil
}

// Transform runs fn on the raw overlay/providers document; apply=false validates but writes nothing.
func Transform(apply bool, fn func(top map[string]json.RawMessage, tx *vault.Tx) error) (oldDoc, newDoc []byte, err error) {
	return providers.UpdateOverlayDoc(apply, fn)
}

// profilesOf decodes the document's profiles section (zero when absent).
func profilesOf(top map[string]json.RawMessage) (providers.Profiles, error) {
	var p providers.Profiles
	if raw, ok := top["profiles"]; ok {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("vault overlay/providers: profiles: %w", err)
		}
	}
	return p, nil
}

// putProfiles writes p back as the profiles section, or drops the key when it is empty.
func putProfiles(top map[string]json.RawMessage, p providers.Profiles) error {
	if p.Active == "" && len(p.Sets) == 0 {
		delete(top, "profiles")
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	top["profiles"] = b
	return nil
}

// mutateProfiles applies fn to the overlay's profiles section; apply=false validates but writes nothing.
func mutateProfiles(apply bool, fn func(*providers.Profiles) error) (oldDoc, newDoc []byte, err error) {
	return Transform(apply, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		p, err := profilesOf(top)
		if err != nil {
			return err
		}
		if err := fn(&p); err != nil {
			return err
		}
		return putProfiles(top, p)
	})
}

// Use sets profiles.active to name; the set must already exist in the overlay.
func Use(apply bool, name string) (oldDoc, newDoc []byte, err error) {
	return mutateProfiles(apply, func(p *providers.Profiles) error {
		if _, ok := p.Sets[name]; !ok {
			return fmt.Errorf("no profile set %q (see `claude-code profile list`)", name)
		}
		p.Active = name
		return nil
	})
}

// Off clears profiles.active and keeps every set.
func Off(apply bool) (oldDoc, newDoc []byte, err error) {
	return mutateProfiles(apply, func(p *providers.Profiles) error {
		p.Active = ""
		return nil
	})
}

// Save stores set under name; an existing set is replaced only with force.
func Save(name string, set providers.Profile, force bool) error {
	if err := CheckName(name); err != nil {
		return err
	}
	_, _, err := mutateProfiles(true, func(p *providers.Profiles) error {
		if _, ok := p.Sets[name]; ok && !force {
			return fmt.Errorf("profile %q exists (use --force to overwrite)", name)
		}
		sets := maps.Clone(p.Sets)
		if sets == nil {
			sets = map[string]providers.Profile{}
		}
		sets[name] = set
		p.Sets = sets
		return nil
	})
	return err
}
