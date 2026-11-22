package databricks

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vault"
)

// hashCacheRE matches the per-SP token caches 742fa4a3 sealed; the profile's _M2M_ACCESS replaced them.
var hashCacheRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^DATABRICKS_M2M_[0-9A-F]{16}_ACCESS$`) })

// foldMove folds the m2m row From into the profile To that pairs the same host and service principal.
type foldMove struct {
	From, To    string
	Secret, Dup bool     // From holds an M2M secret; Dup: To holds one too (dropped if equal, compared at apply)
	Pins        []string // "agent <name>" / "pin <key>" re-pointed From -> To
}

// foldPlan is every change --fold-legacy makes; Kept names the m2m rows that stay and why.
type foldPlan struct {
	Moves  []foldMove
	Kept   []string
	Caches []string
}

func (f foldPlan) empty() bool { return len(f.Moves) == 0 && len(f.Caches) == 0 }

// deleted names every secret applyFold removes: each moved or dropped source secret, then the token caches.
func (f foldPlan) deleted() []string {
	var out []string
	for _, m := range f.Moves {
		if m.Secret {
			out = append(out, secretName(m.From, providers.AuthM2M))
		}
	}
	return append(out, f.Caches...)
}

// foldLegacy is explicit import work, never a registry-load migration: plan, apply when asked, drop folded rows.
func foldLegacy(top map[string]json.RawMessage, tx *vault.Tx, key *vault.Key, apply bool) (foldPlan, error) {
	plan, err := planFold(top, tx)
	if err != nil {
		return plan, err
	}
	if apply {
		if err := applyFold(tx, key, plan); err != nil {
			return plan, err
		}
	}
	provs := existingRows(top)
	for _, m := range plan.Moves {
		delete(provs, m.From)
	}
	keys := providers.ObjectKeys(top["providers"])
	keys = slices.DeleteFunc(keys, func(k string) bool { _, ok := provs[k]; return !ok })
	top["providers"], err = providers.MarshalOrdered(keys, provs)
	return plan, err
}

// foldCredVeto names a non-vault credential source fold would delete with the row; "" when none.
func foldCredVeto(p providers.Provider) string {
	a := p.Auth
	if a.Env != "" || a.File != "" || a.Command != "" || a.RefreshCommand != "" {
		return "takes its credential from auth.env, file or command; move it to the profile first"
	}
	return ""
}

// planFold pairs each Databricks m2m row with the one oauth profile on its host whose spClientId is its clientId.
func planFold(top map[string]json.RawMessage, tx *vault.Tx) (foldPlan, error) {
	rows := decodeRows(existingRows(top))
	settings, err := settingsText()
	if err != nil {
		return foldPlan{}, err
	}
	var plan foldPlan
	claimed := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		if row := rows[id]; row.Auth.Type != providers.AuthM2M || row.Preset != providers.PresetDatabricks {
			continue
		}
		to, why := foldDecision(top, tx, settings, rows, id)
		if why != "" {
			plan.Kept = append(plan.Kept, id+": "+why)
			continue
		}
		mv, err := newFoldMove(tx, id, to, claimed)
		if err != nil {
			return foldPlan{}, err
		}
		plan.Moves = append(plan.Moves, mv)
		if cache := secretName(id, providers.AuthM2M) + "_ACCESS"; hasSecret(tx, cache) {
			plan.Caches = append(plan.Caches, cache)
		}
	}
	if len(plan.Moves) > 0 { // the hash names carry no row id, so they go only with a fold
		plan.Caches = append(plan.Caches, hashCaches(tx)...)
	}
	return plan, nil
}

// hashCaches lists the vault's per-SP hash-named token caches.
func hashCaches(tx *vault.Tx) []string {
	var out []string
	for _, n := range tx.Names(vault.Secrets) {
		if hashCacheRE().MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

// foldDecision is the profile id folds into, else "" and why the row stays.
func foldDecision(top map[string]json.RawMessage, tx *vault.Tx, settings string, rows map[string]providers.Provider, id string) (string, string) {
	to, why := foldTarget(rows, id)
	if why == "" {
		why = cmp.Or(foldCredVeto(rows[id]), foldVeto(top, tx, settings, id))
	}
	return to, why
}

// newFoldMove is the move of id into to; claimed records destinations this plan's moves already fill.
func newFoldMove(tx *vault.Tx, id, to string, claimed map[string]bool) (foldMove, error) {
	pins, err := pinsOf(tx, id)
	if err != nil {
		return foldMove{}, err
	}
	src, dst := secretName(id, providers.AuthM2M), secretName(to, providers.AuthM2M)
	has, dup := hasSecret(tx, src), hasSecret(tx, dst)
	mv := foldMove{From: id, To: to, Secret: has, Dup: has && (dup || claimed[dst]), Pins: pins}
	claimed[dst] = claimed[dst] || has
	return mv, nil
}

// hasSecret reports whether the vault transaction holds the named secret.
func hasSecret(tx *vault.Tx, name string) bool {
	_, ok := tx.Get(vault.Secrets, name)
	return ok
}

// decodeRows decodes each overlay row; an undecodable row is skipped (validation reports it elsewhere).
func decodeRows(raw map[string]json.RawMessage) map[string]providers.Provider {
	out := map[string]providers.Provider{}
	for id, b := range raw {
		var p providers.Provider
		if json.Unmarshal(b, &p) == nil {
			out[id] = p
		}
	}
	return out
}

// foldTarget is the one profile id folds into, else "" and why it stays.
func foldTarget(rows map[string]providers.Provider, id string) (string, string) {
	src := rows[id]
	var hits []string
	for to, p := range rows {
		if to != id && p.Auth.Type == providers.AuthOAuth && p.Auth.SPClientID != "" &&
			p.Auth.SPClientID == src.Auth.ClientID && sameDatabricksHost(&src, &p) &&
			sameWorkspace(src.Auth.WorkspaceID, p.Auth.WorkspaceID) {
			hits = append(hits, to)
		}
	}
	slices.Sort(hits)
	switch len(hits) {
	case 0:
		return "", "no profile pairs its host and client id"
	case 1:
		return hits[0], ""
	}
	return "", "several profiles pair it: " + strings.Join(hits, ", ")
}

func sameWorkspace(a, b string) bool {
	return a == "" || b == "" || a == b
}

// foldVeto names what keeps id from folding: a non-M2M secret or a reference fold cannot re-point; "" when none.
func foldVeto(top map[string]json.RawMessage, tx *vault.Tx, settings, id string) string {
	for _, m := range []string{providers.AuthOAuth, providers.AuthPAT} {
		if hasSecret(tx, secretName(id, m)) {
			return "holds " + secretName(id, m) + "; move or delete it first"
		}
	}
	quoted := strconv.Quote(id)
	if why := docRef(top, quoted, id); why != "" {
		return why
	}
	if why := vaultRef(tx, quoted); why != "" {
		return why
	}
	if strings.Contains(settings, quoted) {
		return "referenced by a settings file"
	}
	return ""
}

// docRef finds id in the overlay doc outside its own row: another top-level key or another row.
func docRef(top map[string]json.RawMessage, quoted, id string) string {
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if k != "providers" && strings.Contains(string(top[k]), quoted) {
			return "referenced by overlay " + k
		}
	}
	rows := existingRows(top)
	for _, other := range slices.Sorted(maps.Keys(rows)) {
		if other != id && strings.Contains(string(rows[other]), quoted) {
			return "referenced by row " + other
		}
	}
	return ""
}

// vaultRef finds id in vault overlay entries fold does not own, or as a vault env value.
func vaultRef(tx *vault.Tx, quoted string) string {
	owned := []string{"providers", "providers.prev", vault.AgentRecordsKey}
	overlay := tx.All(vault.Overlay)
	for _, k := range slices.Sorted(maps.Keys(overlay)) {
		if !slices.Contains(owned, k) && strings.Contains(overlay[k], quoted) {
			return "referenced by vault overlay " + k
		}
	}
	id, _ := strconv.Unquote(quoted)
	word := regexp.MustCompile(`(^|[^A-Za-z0-9])` + regexp.QuoteMeta(id) + `([^A-Za-z0-9]|$)`)
	env := tx.All(vault.Env)
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if word.MatchString(k) || word.MatchString(env[k]) {
			return "referenced by vault env " + k
		}
	}
	return ""
}

// settingsText is settings.json and settings.local.json concatenated; a missing file is empty.
func settingsText() (string, error) {
	var b strings.Builder
	for _, name := range []string{"settings.json", "settings.local.json"} {
		data, err := os.ReadFile(filepath.Join(paths.ConfigDir(), name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		b.Write(data)
	}
	return b.String(), nil
}

func pinsOf(tx *vault.Tx, id string) ([]string, error) {
	recs, err := tx.AgentRecords()
	if err != nil {
		return nil, err
	}
	var out []string
	for n, r := range recs {
		_, fields := repointRecord(r, id, "")
		for _, f := range fields {
			out = append(out, "agent "+n+f)
		}
	}
	for k, v := range tx.All(vault.Pins) {
		if strings.TrimSpace(v) == id {
			out = append(out, "pin "+k)
		}
	}
	slices.Sort(out)
	return out, nil
}

// applyFold moves or drops each folded row's M2M secret, re-points its pins and deletes the dead token caches.
func applyFold(tx *vault.Tx, key *vault.Key, plan foldPlan) error {
	for _, m := range plan.Moves {
		if err := foldSecret(tx, key, m); err != nil {
			return err
		}
		if err := repoint(tx, m.From, m.To); err != nil {
			return err
		}
	}
	for _, n := range plan.Caches {
		if err := tx.Put(vault.Secrets, n, ""); err != nil {
			return err
		}
	}
	return nil
}

// foldSecret moves From's M2M secret to To's name, or drops it when To already seals the same value.
func foldSecret(tx *vault.Tx, key *vault.Key, m foldMove) error {
	if !m.Secret {
		return nil
	}
	if key == nil {
		return vault.ErrLocked
	}
	if kid, _ := tx.Get(vault.Meta, "kid"); kid != key.Kid {
		return vault.ErrWrongKey
	}
	src, dst := secretName(m.From, providers.AuthM2M), secretName(m.To, providers.AuthM2M)
	if !hasSecret(tx, dst) {
		return moveLegacySecret(tx, key, src, dst)
	}
	a, err := openSecret(tx, key, src)
	if err != nil {
		return err
	}
	b, err := openSecret(tx, key, dst)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("fold %s: %s differs from %s; keep the right one in %s, delete %s, re-run", m.From, src, dst, dst, src)
	}
	return tx.Put(vault.Secrets, src, "")
}

func openSecret(tx *vault.Tx, key *vault.Key, name string) (string, error) {
	record, _, err := tx.Record(name)
	if err != nil {
		return "", err
	}
	return key.Open(name, record)
}

func repoint(tx *vault.Tx, from, to string) error {
	recs, err := tx.AgentRecords()
	if err != nil {
		return err
	}
	next, changed := maps.Clone(recs), false
	for n, r := range recs {
		if moved, fields := repointRecord(r, from, to); len(fields) > 0 {
			next[n], changed = moved, true
		}
	}
	if changed {
		if err := tx.PutAgentRecords(next); err != nil {
			return err
		}
	}
	for k, v := range tx.All(vault.Pins) {
		if strings.TrimSpace(v) == from {
			if err := tx.Put(vault.Pins, k, to); err != nil {
				return err
			}
		}
	}
	return nil
}

// repointRecord moves Databricks route profiles and legacy model prefixes, preserving each route's other keys.
func repointRecord(r vault.AgentRecord, from, to string) (vault.AgentRecord, []string) {
	var fields []string
	spec := func(s, field string) string {
		if head, rest, ok := strings.Cut(s, ":"); ok && head == from {
			target := route.Target{Model: rest}
			target.Provider, target.Profile = providers.GroupDatabricks, to
			fields = append(fields, field)
			return target.Profile + ":" + target.Model // vault model and alias fields still store legacy strings
		}
		return s
	}
	if rt := r.EffectiveRoute(); rt.Profile == from {
		rt.Provider, rt.Profile = cmp.Or(rt.Provider, providers.GroupDatabricks), to
		r.Route = rt
		fields = append(fields, "")
	}
	if rt := r.EffectiveFallbackRoute(); rt.Profile == from {
		rt.Provider, rt.Profile = cmp.Or(rt.Provider, providers.GroupDatabricks), to
		r.FallbackRoute = rt
		fields = append(fields, " fallback")
	}
	r.Model = spec(r.Model, " model")
	aliases := maps.Clone(r.Aliases)
	for _, a := range slices.Sorted(maps.Keys(r.Aliases)) {
		aliases[a] = spec(r.Aliases[a], " alias "+a)
	}
	r.Aliases = aliases
	return r, fields
}

// legacyFoldKey loads the master key only when a folded row holds a secret to move or compare.
func legacyFoldKey(plan foldPlan) (*vault.Key, error) {
	if !slices.ContainsFunc(plan.Moves, func(m foldMove) bool { return m.Secret }) {
		return nil, nil
	}
	key, _, err := vault.MasterKey()
	return key, err
}

// printFold lists every row, secret, pin and cache --fold-legacy changes; secret values are never printed.
func printFold(w io.Writer, plan foldPlan) {
	for _, m := range plan.Moves {
		fmt.Fprintf(w, "fold %s -> %s\n", m.From, m.To)
		src, dst := secretName(m.From, providers.AuthM2M), secretName(m.To, providers.AuthM2M)
		switch {
		case m.Dup:
			fmt.Fprintf(w, "  drop %s (same as %s; compared at apply)\n", src, dst)
		case m.Secret:
			fmt.Fprintf(w, "  move %s -> %s\n", src, dst)
		}
		for _, p := range m.Pins {
			fmt.Fprintf(w, "  %s: %s -> %s\n", p, m.From, m.To)
		}
	}
	for _, k := range plan.Kept {
		fmt.Fprintf(w, "keep %s\n", k)
	}
	for _, c := range plan.Caches {
		fmt.Fprintf(w, "drop cache %s\n", c)
	}
}

// sameDatabricksHost reports whether both rows are Databricks presets on the same valid https host.
func sameDatabricksHost(from, to *providers.Provider) bool {
	host, ok := normalizeHost(providers.AuthHost(from))
	dest, valid := normalizeHost(providers.AuthHost(to))
	return ok && valid && host == dest && from.Preset == providers.PresetDatabricks && to.Preset == providers.PresetDatabricks
}

// moveLegacySecret reseals the secret record src under dst's name and blanks src.
func moveLegacySecret(tx *vault.Tx, key *vault.Key, src, dst string) error {
	value, err := openSecret(tx, key, src)
	if err != nil {
		return err
	}
	record, _, err := tx.Record(src)
	if err != nil {
		return err
	}
	record.Sealed = key.Seal(dst, value)
	if err := tx.PutRecord(dst, record); err != nil {
		return err
	}
	return tx.Put(vault.Secrets, src, "")
}
