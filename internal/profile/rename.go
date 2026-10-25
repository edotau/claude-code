package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

// DefaultSet is the owner's profile set; it replaces the per-workspace databricks-<env> sets.
const DefaultSet = "DEFAULT"

// legacyDev is the set DEFAULT is copied from.
const legacyDev = "databricks-dev"

// SealedMove is one sealed secret that follows its row to the new id.
type SealedMove struct{ From, To string }

// renameDoc renames preset rows databricks-<env> to <env>, re-points every set row at them, and replaces the
// databricks-<env> sets with DEFAULT (a copy of databricks-dev); own and other sets are kept.
func renameDoc(top map[string]json.RawMessage, pins, env map[string]string, vendors string, sealed []string) (
	[]string, []SealedMove, error) {
	provs := map[string]json.RawMessage{}
	if raw, ok := top["providers"]; ok {
		if err := json.Unmarshal(raw, &provs); err != nil {
			return nil, nil, fmt.Errorf("vault overlay/providers: providers: %w", err)
		}
	}
	prof, err := profilesOf(top)
	if err != nil {
		return nil, nil, err
	}
	renamed, moves, notes, problems := planRenames(top, provs, pins, env, vendors, sealed)
	if slices.Contains(legacySets, prof.Active) {
		problems = append(problems, fmt.Sprintf("profiles.active is %s; run `claude-code profile off` first", prof.Active))
	}
	if len(problems) > 0 {
		return nil, nil, errors.New("cannot rename; still referenced:\n  " + strings.Join(problems, "\n  "))
	}
	for o, n := range renamed {
		provs[n] = provs[o]
		delete(provs, o)
		notes = append(notes, fmt.Sprintf("row %s -> %s", o, n))
		if os.Getenv(providers.PinProvider) == o {
			notes = append(notes, fmt.Sprintf("%s=%s is exported in your shell; it resolves for one release, then set it to %s",
				providers.PinProvider, o, n))
		}
	}
	for _, m := range moves {
		notes = append(notes, fmt.Sprintf("sealed %s -> %s", m.From, m.To))
	}
	sets, setNotes := renameSets(prof.Sets, renamed, provs, pins)
	notes = append(notes, setNotes...)
	if len(renamed) == 0 && len(setNotes) == 0 {
		notes = append(notes, "rows and sets already use dev, qa, prod and DEFAULT")
	}
	if top["providers"], err = json.Marshal(provs); err != nil {
		return nil, nil, err
	}
	prof.Sets = sets
	slices.Sort(notes)
	return notes, moves, putProfiles(top, prof)
}

// legacySets are the per-workspace sets DEFAULT replaces.
var legacySets = func() []string {
	var out []string
	for _, e := range providers.DatabricksEnvs {
		out = append(out, "databricks-"+e)
	}
	return out
}()

// planRenames picks the rows to rename and the sealed secrets to move; problems block the whole rename.
func planRenames(top, provs map[string]json.RawMessage, pins, env map[string]string, vendors string, sealed []string) (
	renamed map[string]string, moves []SealedMove, notes, problems []string) {
	renamed = map[string]string{}
	rest := maps.Clone(top)
	delete(rest, "profiles") // set rows are rewritten below, not dangling
	for _, e := range providers.DatabricksEnvs {
		o := "databricks-" + e
		raw, ok := provs[o]
		if !ok {
			continue
		}
		var row providers.Provider
		if err := json.Unmarshal(raw, &row); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", o, err))
			continue
		}
		switch {
		case row.Preset != providers.PresetDatabricks:
			notes = append(notes, fmt.Sprintf("skip rename %s: not a databricks preset row", o))
			continue
		case provs[e] != nil:
			problems = append(problems, fmt.Sprintf("row %s already exists; cannot rename %s", e, o))
			continue
		}
		if refs := findRefs(rest, provs, pins, env, vendors, o); len(refs) > 0 {
			problems = append(problems, refs...)
			continue
		}
		renamed[o] = e
		from := providers.SecretNameFor(o, row.Auth.AuthType)
		if from == "" || !slices.Contains(sealed, from) {
			continue
		}
		to := providers.SecretNameFor(e, row.Auth.AuthType)
		if slices.Contains(sealed, to) {
			problems = append(problems, fmt.Sprintf("sealed %s already exists; cannot move %s", to, from))
			continue
		}
		moves = append(moves, SealedMove{From: from, To: to})
	}
	return renamed, moves, notes, problems
}

// renameSets drops the legacy sets for DEFAULT and re-points every remaining row's account.
func renameSets(old map[string]providers.Profile, renamed map[string]string, provs map[string]json.RawMessage,
	pins map[string]string) (map[string]providers.Profile, []string) {
	sets := map[string]providers.Profile{}
	var notes []string
	for name, set := range old {
		if slices.Contains(legacySets, name) {
			notes = append(notes, "removed set "+name)
			continue
		}
		sets[name] = renameAccounts(set, renamed)
	}
	if len(notes) == 0 && len(renamed) == 0 {
		return sets, nil
	}
	switch {
	case hasSet(sets, DefaultSet):
		notes = append(notes, "kept existing set "+DefaultSet)
	case hasSet(old, legacyDev):
		sets[DefaultSet] = renameAccounts(old[legacyDev], renamed)
		notes = append(notes, "created set "+DefaultSet+" from "+legacyDev+" (accounts -> dev)")
	case provs["dev"] != nil:
		var row providers.Provider
		_ = json.Unmarshal(provs["dev"], &row)
		sets[DefaultSet] = envSet("dev", row.Models, pins)
		notes = append(notes, "created set "+DefaultSet+" on row dev")
	}
	if _, ok := sets["own"]; !ok {
		sets["own"] = providers.Profile{}
		notes = append(notes, "created empty set own")
	}
	return sets, notes
}

func hasSet(sets map[string]providers.Profile, name string) bool {
	_, ok := sets[name]
	return ok
}

// renameAccounts returns a copy of set with accounts (and provider:model specs) moved to their new row ids.
func renameAccounts(set providers.Profile, renamed map[string]string) providers.Profile {
	out := providers.Profile{}
	for agent, row := range set {
		if n, ok := renamed[row.Account]; ok {
			row.Account = n
		}
		if p, m, ok := strings.Cut(row.Model, ":"); ok && renamed[p] != "" {
			row.Model = renamed[p] + ":" + m
		}
		if len(row.Models) > 0 {
			ms := maps.Clone(row.Models)
			for slot, spec := range ms {
				if p, m, ok := strings.Cut(spec, ":"); ok && renamed[p] != "" {
					ms[slot] = renamed[p] + ":" + m
				}
			}
			row.Models = ms
		}
		out[agent] = row
	}
	return out
}

// moveSealed re-seals each moved secret under its new name, then deletes the old record.
func moveSealed(moves []SealedMove) error {
	for _, m := range moves {
		v, found, err := vault.ReadSecret(m.From)
		if err != nil {
			return fmt.Errorf("read %s: %w", m.From, err)
		}
		if !found {
			continue
		}
		if err := secrets.Store(m.To, v); err != nil {
			return fmt.Errorf("seal %s: %w", m.To, err)
		}
		if err := secrets.Delete(m.From); err != nil {
			return fmt.Errorf("delete %s (now sealed as %s): %w", m.From, m.To, err)
		}
	}
	return nil
}
