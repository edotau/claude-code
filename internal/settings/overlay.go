package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/vault"
)

// overlayBucketKey is where the per-machine settings page overrides live: vault bucket
// Overlay, plain (not sealed), one JSON object of catalog key -> value.
const overlayBucketKey = "settings"

// dropBucketKey is the vault bucket Overlay entry holding top-level settings.json keys removed on every render.
const dropBucketKey = "settings-drop"

// LoadDropped reads the drop list; an absent or empty entry is nil.
func LoadDropped() ([]string, error) {
	var keys []string
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, dropBucketKey)
		if !ok || raw == "" {
			return nil
		}
		return json.Unmarshal([]byte(raw), &keys)
	})
	if err != nil {
		return nil, fmt.Errorf("vault overlay/%s: %w", dropBucketKey, err)
	}
	return keys, nil
}

// CheckDroppable refuses any key the template owns (catalogued or rendered at top level): a render puts it back.
func CheckDroppable(keys []string) error {
	rendered, err := Render()
	if err != nil {
		return err
	}
	tp, err := parseObject(rendered)
	if err != nil {
		return err
	}
	for _, k := range keys {
		_, catalogued := Lookup(k)
		if _, rendered := tp.vals[k]; catalogued || rendered {
			return fmt.Errorf("%s is owned by the template; edit defaults/claude/settings.json", k)
		}
	}
	return nil
}

// UpdateDropped applies next to the stored list inside one vault transaction (sorted, deduped, checked);
// an empty result deletes the entry instead of storing "[]".
func UpdateDropped(next func(cur []string) []string) error {
	return vault.Update(func(t *vault.Tx) error {
		var cur []string
		if raw, ok := t.Get(vault.Overlay, dropBucketKey); ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &cur); err != nil {
				return fmt.Errorf("vault overlay/%s: %w", dropBucketKey, err)
			}
		}
		keys := slices.Compact(slices.Sorted(slices.Values(next(cur))))
		if err := CheckDroppable(keys); err != nil {
			return err
		}
		var raw string
		if len(keys) > 0 {
			b, err := json.Marshal(keys)
			if err != nil {
				return err
			}
			raw = string(b)
		}
		return t.Put(vault.Overlay, dropBucketKey, raw)
	})
}

// LoadOverlay reads the vault overlay/settings bucket; an absent entry is an empty map, not an error.
func LoadOverlay() (map[string]json.RawMessage, error) {
	ov := map[string]json.RawMessage{}
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, overlayBucketKey)
		if !ok || raw == "" {
			return nil
		}
		return json.Unmarshal([]byte(raw), &ov)
	})
	if err != nil {
		return nil, fmt.Errorf("vault overlay/settings: %w", err)
	}
	return ov, nil
}

// ValidateOverlay checks every key against the Catalog, else the schema, and runs its Validate.
func ValidateOverlay(ov map[string]json.RawMessage) error {
	for k, v := range ov {
		if err := validateKey(k, v); err != nil {
			return fmt.Errorf("vault overlay/settings: %s: %w", k, err)
		}
	}
	return nil
}

// validateKey checks v against the catalog entry for k, else the schema entry; an unknown key is an error.
func validateKey(k string, v json.RawMessage) error {
	if err := checkNotCredential(k, v); err != nil {
		return err
	}
	if k == "autoMemoryDirectory" {
		return errors.New("autoMemoryDirectory is retired: it merges every repo's memory into one directory")
	}
	if structuralKey(k) {
		return fmt.Errorf("%s is generated or structural; edit defaults/claude/settings.json by hand", k)
	}
	if f, ok := Lookup(k); ok {
		return f.Validate(v)
	}
	if sk, ok := SchemaKey(k); ok {
		return sk.Validate(v)
	}
	return errors.New("unknown key")
}

// SaveOverlay validates every key (Catalog or schema), then writes ov whole; an empty map deletes
// the vault entry instead of storing "{}".
func SaveOverlay(ov map[string]json.RawMessage) error {
	if err := ValidateOverlay(ov); err != nil {
		return err
	}
	var raw string
	if len(ov) > 0 {
		b, err := json.Marshal(ov)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	return vault.Update(func(t *vault.Tx) error {
		return t.Put(vault.Overlay, overlayBucketKey, raw)
	})
}

// RenderLive is Render with the vault overlay applied on top.
func RenderLive() ([]byte, error) {
	rendered, err := Render()
	if err != nil {
		return nil, err
	}
	ov, err := LoadOverlay()
	if err != nil {
		return nil, err
	}
	return applyOverlay(rendered, ov)
}

// applyOverlay is pure: it never touches the vault, so tests can drive it with fixed inputs.
// An unknown key or a value Validate rejects is an error naming the key and the reason.
func applyOverlay(rendered []byte, ov map[string]json.RawMessage) ([]byte, error) {
	o, err := parseObject(rendered)
	if err != nil {
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(ov)) {
		v := ov[k]
		if err := validateKey(k, v); err != nil {
			return nil, fmt.Errorf("vault overlay/settings: %s: %w", k, err)
		}
		if err := setKey(o, k, v); err != nil {
			return nil, fmt.Errorf("vault overlay/settings: %s: %w", k, err)
		}
	}
	hooks, err := SettingsJSON() // defence in depth: the generated hooks always win
	if err != nil {
		return nil, err
	}
	o.set("hooks", hooks)
	return o.marshal()
}

// setKey sets a top-level key, or for a schema env variable puts it inside the env object.
func setKey(o *object, k string, v json.RawMessage) error {
	if sk, ok := SchemaKey(k); !ok || !sk.Env {
		o.set(k, v)
		return nil
	}
	env, err := parseObject(orEmpty(o.vals["env"]))
	if err != nil {
		return fmt.Errorf("env: %w", err)
	}
	env.set(k, v)
	raw, err := env.marshal()
	if err != nil {
		return err
	}
	o.set("env", raw)
	return nil
}

// structuralKey names the keys the harness generates or owns whole: no overlay, picker or repo-default edit.
func structuralKey(k string) bool {
	switch k {
	case "hooks", "statusLine", "$schema", "permissions", "env", "autoMemoryDirectory":
		return true
	}
	return false
}

// StructuralKey reports a key the page and the writers refuse to edit (env variables stay editable one by one).
func StructuralKey(k string) bool { return structuralKey(k) }

// hooksBucketKey is the vault Overlay entry holding the harness hook-group switches: group name -> enabled.
const hooksBucketKey = "harnessHooks"

// LoadHarnessHooks reads the group switches; an absent entry is an empty map (every group on).
func LoadHarnessHooks() (map[string]bool, error) {
	m := map[string]bool{}
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, hooksBucketKey)
		if !ok || raw == "" {
			return nil
		}
		return json.Unmarshal([]byte(raw), &m)
	})
	if err != nil {
		return nil, fmt.Errorf("vault overlay/%s: %w", hooksBucketKey, err)
	}
	// The docs group was named plans; a stored plans switch is vault state, so it is read as docs.
	if on, ok := m["plans"]; ok {
		if _, set := m["docs"]; !set {
			m["docs"] = on
		}
		delete(m, "plans")
	}
	return m, nil
}

// SaveHarnessHooks validates group names (a gate group cannot be switched off) and stores m; empty deletes the entry.
func SaveHarnessHooks(m map[string]bool) error {
	for name, on := range m {
		var found, gate bool
		for _, g := range Registry {
			if g.Name == name {
				found, gate = true, g.Gate
			}
		}
		if !found {
			return fmt.Errorf("harnessHooks: unknown group %q", name)
		}
		if gate && !on {
			return fmt.Errorf("harnessHooks: %s is a gate group and stays on", name)
		}
	}
	var raw string
	if len(m) > 0 {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	return vault.Update(func(t *vault.Tx) error { return t.Put(vault.Overlay, hooksBucketKey, raw) })
}

// camelSecret matches settings names that hold or fetch a credential (apiKeyHelper, awsCredentialExport).
var camelSecret = regexp.MustCompile(`(?i)(apikey|credential|secret|password|token)`)

// authHeader matches a literal credential-bearing HTTP header at the start of any line or "\\n"-joined segment.
var authHeader = regexp.MustCompile(`(?i)(^|[\r\n]|\\n)\s*(proxy-authorization|authorization|x-api-key|api-key|cookie)\s*:`)

// CredentialKey reports a key whose value is a credential by name: it must never land in a plain file.
func CredentialKey(name string) bool {
	if !strings.Contains(name, "_") {
		return camelSecret.MatchString(name)
	}
	for seg := range strings.SplitSeq(strings.ToUpper(name), "_") {
		switch seg {
		case "TTL", "MS":
			return false
		}
	}
	for seg := range strings.SplitSeq(strings.ToUpper(name), "_") {
		switch seg {
		case "KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIALS":
			return true
		}
	}
	return false
}

// checkNotCredential refuses a credential-named key or a credential-shaped string value.
func checkNotCredential(key string, raw json.RawMessage) error {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if CredentialKey(key) || credentialIn(v) {
		return fmt.Errorf("%s looks like a credential; store it with `claude-code vault set` or a provider auth.command", key)
	}
	return nil
}

// credentialIn walks nested values: a credential-named key or a credential-shaped string at any depth.
func credentialIn(v any) bool {
	switch t := v.(type) {
	case string:
		return authHeader.MatchString(t) || shapes.Looks(t) || slices.ContainsFunc(strings.FieldsFunc(t, splitToken), shapes.Looks)
	case []any:
		return slices.ContainsFunc(t, credentialIn)
	case map[string]any:
		for k, x := range t {
			if CredentialKey(k) || credentialIn(x) {
				return true
			}
		}
	}
	return false
}

// splitToken separates the words of a string value: whitespace, ':', '=', ',', ';' and quotes.
func splitToken(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(`:=,;"'`, r)
}

// RepoDefaultsRel is the tracked repo defaults file, relative to the config dir.
const RepoDefaultsRel = "defaults/claude/settings.json"

// RepoDefaultsFile is the path of the repo defaults file in the live config dir.
func RepoDefaultsFile() string {
	return filepath.Join(paths.ConfigDir(), filepath.FromSlash(RepoDefaultsRel))
}

// WriteRepoDefaults merges staged into the repo defaults file (env variables go under env), keeping
// $schema first and 2-space JSON; it returns the path written.
func WriteRepoDefaults(staged map[string]json.RawMessage) (string, error) {
	path := RepoDefaultsFile()
	err := paths.WithFileLock(path, func() error { return mergeRepoDefaults(path, staged) })
	return path, err
}

// mergeRepoDefaults is the read-modify-write, run under the file lock and replaced atomically.
func mergeRepoDefaults(path string, staged map[string]json.RawMessage) error {
	cur, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	o, err := parseObject(cur)
	if err != nil {
		return fmt.Errorf("%s: %w", RepoDefaultsRel, err)
	}
	for _, k := range slices.Sorted(maps.Keys(staged)) {
		if err := validateKey(k, staged[k]); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		if err := setKey(o, k, staged[k]); err != nil {
			return err
		}
	}
	if i := slices.Index(o.keys, "$schema"); i > 0 {
		o.keys = slices.Concat([]string{"$schema"}, o.keys[:i], o.keys[i+1:])
	}
	out, err := o.marshal()
	if err != nil {
		return err
	}
	if bytes.Equal(out, cur) {
		return nil
	}
	return paths.AtomicWrite(path, out, 0o644)
}
