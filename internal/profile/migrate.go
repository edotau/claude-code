package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

var profileFlagRE = regexp.MustCompile(`(?:^|\s)-p\s+([A-Za-z0-9_-]+)`)

// deriveBase is a minimal valid registry the preset is expanded against.
const deriveBase = `{"default":"d","providers":{"d":{"kind":"anthropic","base_url":"https://d.invalid",` +
	`"auth":{"type":"none"},"models":{"opus":"m"}}}}`

// Migrate folds each databricks-<env>[-oai] twin pair into a preset row, renames those rows to <env> and
// their sets to DEFAULT, and moves any sealed secret along; never sets profiles.active.
func Migrate(apply bool) (oldDoc, newDoc []byte, notes []string, err error) {
	sealed, err := secrets.NamesErr()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("vault secrets: %w", err)
	}
	var moves []SealedMove
	oldDoc, newDoc, err = Transform(apply, func(top map[string]json.RawMessage, tx *vault.Tx) error {
		vendors, _ := tx.Get(vault.Overlay, "vendors")
		pins, env := tx.All(vault.Pins), tx.All(vault.Env)
		var e error
		if notes, e = migrateDoc(top, pins, env, vendors); e != nil {
			return e
		}
		more, mv, e := renameDoc(top, pins, env, vendors, sealed)
		notes, moves = append(notes, more...), mv
		return e
	})
	if err == nil && apply {
		err = moveSealed(moves)
	}
	return oldDoc, newDoc, notes, err
}

func migrateDoc(top map[string]json.RawMessage, pins, env map[string]string, vendors string) ([]string, error) {
	provs := map[string]json.RawMessage{}
	if raw, ok := top["providers"]; ok {
		if err := json.Unmarshal(raw, &provs); err != nil {
			return nil, fmt.Errorf("vault overlay/providers: providers: %w", err)
		}
	}
	prof, err := profilesOf(top)
	if err != nil {
		return nil, err
	}
	sets := maps.Clone(prof.Sets)
	if sets == nil {
		sets = map[string]providers.Profile{}
	}
	var notes, problems []string
	for _, e := range providers.DatabricksEnvs {
		a, o := "databricks-"+e, "databricks-"+e+"-oai"
		rawA, hasA := provs[a]
		_, hasO := provs[o]
		if hasA && !hasO && presetOf(rawA) == providers.PresetDatabricks {
			continue // already folded by an earlier migrate
		}
		if !hasA || !hasO {
			notes = append(notes, fmt.Sprintf("skip %s: needs both %s and %s in the overlay", e, a, o))
			continue
		}
		if refs := findRefs(top, provs, pins, env, vendors, o); len(refs) > 0 {
			problems = append(problems, refs...)
			continue
		}
		entry, set, err := migrateEnv(e, rawA, pins)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		provs[a] = entry
		delete(provs, o)
		if _, exists := sets[a]; exists {
			notes = append(notes, fmt.Sprintf("kept existing set %s", a))
		} else {
			sets[a] = set
		}
		notes = append(notes, fmt.Sprintf("%s -> preset databricks; removed %s", a, o))
		if os.Getenv(providers.PinProvider) == o { // the shell export outlives the vault edit
			notes = append(notes, fmt.Sprintf("%s=%s is exported in your shell; after --apply set it to %s or unset it",
				providers.PinProvider, o, a))
		}
	}
	if len(problems) > 0 {
		return nil, errors.New("cannot migrate; still referenced:\n  " + strings.Join(problems, "\n  "))
	}
	if _, ok := sets["own"]; !ok {
		sets["own"] = providers.Profile{}
		notes = append(notes, "created empty set own")
	}
	if top["providers"], err = json.Marshal(provs); err != nil {
		return nil, err
	}
	prof.Sets = sets
	return notes, putProfiles(top, prof)
}

// presetOf is a raw provider row's preset ("" when absent or unparsable).
func presetOf(raw json.RawMessage) string {
	var p struct {
		Preset string `json:"preset"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Preset
}

// migrateEnv builds the preset row and the profile set for one workspace.
func migrateEnv(env string, raw json.RawMessage, pins map[string]string) (json.RawMessage, providers.Profile, error) {
	name := "databricks-" + env
	var old providers.Provider
	if err := json.Unmarshal(raw, &old); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if old.Preset != "" {
		return nil, nil, fmt.Errorf("%s: already has preset %q (hand-edited; migrate by hand)", name, old.Preset)
	}
	u, err := url.Parse(old.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, nil, fmt.Errorf("%s: base_url %q is not an https URL", name, old.BaseURL)
	}
	host := "https://" + u.Host
	m := profileFlagRE.FindStringSubmatch(old.Auth.Command)
	if m == nil {
		return nil, nil, fmt.Errorf("%s: no `-p <profile>` in auth.command", name)
	}
	entry, err := presetEntry(raw, host, m[1])
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	return entry, envSet(name, old.Models, pins), nil
}

// presetEntry is {preset, host, cliProfile} plus every explicit field of raw that differs from the derived one.
func presetEntry(raw json.RawMessage, host, cliProfile string) (json.RawMessage, error) {
	ov := fmt.Sprintf(`{"providers":{"x":{"preset":"databricks","host":%q,"cliProfile":%q}}}`, host, cliProfile)
	reg, err := providers.Parse([]byte(deriveBase), []byte(ov))
	if err != nil {
		return nil, fmt.Errorf("derive preset: %w", err)
	}
	db, err := json.Marshal(reg.Providers["x"])
	if err != nil {
		return nil, err
	}
	var derived, old map[string]json.RawMessage
	if err := json.Unmarshal(db, &derived); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		return nil, err
	}
	out := explicitOnly(old, derived)
	for k, v := range map[string]string{"preset": providers.PresetDatabricks, "host": host, "cliProfile": cliProfile} {
		out[k], _ = json.Marshal(v)
	}
	return json.Marshal(out)
}

// explicitOnly keeps the keys of old whose value is absent from or different in derived, recursing into objects.
func explicitOnly(old, derived map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, ov := range old {
		dv, ok := derived[k]
		if !ok {
			out[k] = ov
			continue
		}
		var oo, dd map[string]json.RawMessage
		if json.Unmarshal(ov, &oo) == nil && json.Unmarshal(dv, &dd) == nil && oo != nil && dd != nil {
			if sub := explicitOnly(oo, dd); len(sub) > 0 {
				out[k], _ = json.Marshal(sub)
			}
			continue
		}
		if !sameJSON(ov, dv) {
			out[k] = ov
		}
	}
	return out
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return false
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}

// envSet is the profile set a migrated workspace gets; opus keeps [1m] when the user's pins carried it.
func envSet(account string, old map[string]string, pins map[string]string) providers.Profile {
	slots := map[string]string{}
	for _, s := range []string{models.Opus, models.Sonnet, models.Haiku} {
		if id := old[s]; id != "" {
			slots[s] = id
		}
	}
	if opus := slots[models.Opus]; opus != "" {
		for _, k := range []string{providers.SlotPin(models.Opus), providers.PinModel} {
			if p := pins[k]; strings.HasSuffix(p, "[1m]") && models.Strip1M(p) == opus {
				slots[models.Opus] = opus + "[1m]"
			}
		}
	}
	return providers.Profile{
		"session":  {Account: account, Models: slots},
		"copilot":  {Account: account, Model: "databricks-claude-opus-5-5"},
		"opencode": {Account: account, Model: "databricks-claude-opus-5-5"},
		"codex":    {Account: account, Model: "databricks-gpt-5-5"},
	}
}

// findRefs lists every place besides the twin's own row that names o.
func findRefs(top map[string]json.RawMessage, provs map[string]json.RawMessage, pins, env map[string]string, vendors, o string) []string {
	var refs []string
	for k, raw := range top {
		if k == "providers" {
			for n, pr := range provs {
				if n != o && bytes.Contains(pr, []byte(o)) {
					refs = append(refs, fmt.Sprintf("providers.%s names %s", n, o))
				}
			}
			continue
		}
		if bytes.Contains(raw, []byte(o)) {
			refs = append(refs, fmt.Sprintf("%s names %s", k, o))
		}
	}
	for n, v := range pins {
		if strings.Contains(v, o) {
			refs = append(refs, fmt.Sprintf("pin %s names %s", n, o))
		}
	}
	for n, v := range env {
		if strings.Contains(v, o) {
			refs = append(refs, fmt.Sprintf("env %s names %s", n, o))
		}
	}
	if strings.Contains(vendors, o) {
		refs = append(refs, "vendors overlay names "+o)
	}
	slices.Sort(refs)
	return refs
}
