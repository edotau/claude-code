// Package vaultmigrate plans and applies the one-time vault rename to the Phase 6 naming standard.
package vaultmigrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// Kind classifies a Change.
type Kind string

const (
	KindPinKey   Kind = "pin-key"   // a pin renamed (value kept)
	KindPinValue Kind = "pin-value" // a pin value naming a retired profile
	KindSecret   Kind = "secret"    // a sealed secret re-sealed under a new name
	KindOverlay  Kind = "overlay"   // overlay/providers flat -> nested (and retired profile names)
	KindSchema   Kind = "schema"    // meta/schema bump
	KindSkipped  Kind = "skipped"   // recognised but deliberately left alone
)

// Change is one line of the dry-run table. Secret changes carry names and lengths only, never values.
type Change struct {
	Kind    Kind     `json:"kind"`
	Old     string   `json:"old"`
	New     string   `json:"new,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	Sources []string `json:"sources,omitempty"` // KindSecret: old vault names merged into New
	Keep    bool     `json:"keep,omitempty"`    // KindSecret: old name stays (an env @secret ref reads it)
	Len     int      `json:"len,omitempty"`     // KindSecret: byte length of the largest source, when recorded
}

// Plan is the full, value-free change list.
type Plan struct {
	Changes []Change `json:"changes"`
	Skipped []Change `json:"skipped,omitempty"`
}

// Empty reports that nothing is left to migrate.
func (p *Plan) Empty() bool { return len(p.Changes) == 0 }

// NeedsKey reports whether applying re-seals secrets (and so needs the master key).
func (p *Plan) NeedsKey() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool { return c.Kind == KindSecret })
}

// Result is what Apply did.
type Result struct {
	Plan   *Plan  `json:"plan"`
	Backup string `json:"backup,omitempty"`
}

// Current loads the registry the plan resolves profiles with: built-ins plus the vault overlay, converted from any
// old shape in memory (the vault itself is not touched).
func Current() (*providers.Registry, error) {
	doc, err := providers.OverlayDoc()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("vault migrate: %w", err)
	}
	var overlay []byte
	if err == nil {
		d, err := readOverlay(string(doc))
		if err != nil {
			return nil, fmt.Errorf("vault migrate: %w", err)
		}
		fixTopNames(d.top)
		conv, err := d.render()
		if err != nil {
			return nil, fmt.Errorf("vault migrate: %w", err)
		}
		overlay = []byte(conv)
	}
	reg, err := providers.ParseOverlay(overlay)
	if err != nil {
		return nil, fmt.Errorf("vault migrate: load providers: %w", err)
	}
	return reg, nil
}

// Compute builds the change list from the current vault; it reads no secret value and needs no master key.
func Compute(reg *providers.Registry) (*Plan, error) {
	var p *Plan
	err := vault.View(func(tx *vault.Tx) (err error) {
		var dp docPlan
		if dp, err = planDoc(tx, reg); err == nil {
			p = planTx(tx, reg, dp)
		}
		return err
	})
	return p, err
}

// Apply backs the vault up, then performs every change in ONE vault transaction (all or nothing) and bumps the schema.
// A vault with nothing to do (or no vault file) is left untouched and no backup is made.
func Apply(reg *providers.Registry) (*Result, error) {
	plan, err := Compute(reg)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: plan}
	if plan.Empty() {
		return res, nil
	}
	if res.Backup, err = vault.Backup(); err != nil {
		return nil, fmt.Errorf("vault migrate: backup: %w", err)
	}
	run := func(tx *vault.Tx, k *vault.Key) error {
		dp, err := planDoc(tx, reg)
		if err != nil {
			return err
		}
		cur := planTx(tx, reg, dp)
		if !slices.EqualFunc(cur.Changes, plan.Changes, func(a, b Change) bool { return a.Kind == b.Kind && a.Old == b.Old && a.New == b.New }) {
			return errors.New("vault migrate: the vault changed since the plan was made; rerun")
		}
		return execute(tx, reg, k, dp)
	}
	if plan.NeedsKey() {
		err = vault.UpdateSealed(run)
	} else {
		err = vault.Update(func(tx *vault.Tx) error { return run(tx, nil) })
	}
	if err != nil {
		return nil, fmt.Errorf("vault migrate: %w (nothing was changed; backup %s)", err, res.Backup)
	}
	return res, nil
}

// docPlan is the overlay/providers outcome: conv is the converted document, written when write is set.
type docPlan struct {
	conv    string
	write   bool     // the stored document differs from conv
	changed bool     // a structural conversion (flat rows, retired names, auth spelling, a stamped login)
	oneM    []string // profiles whose dead one_m key is dropped
}

// planDoc converts the stored overlay/providers document in memory; it writes nothing.
func planDoc(tx *vault.Tx, reg *providers.Registry) (docPlan, error) {
	raw, _ := tx.Get(vault.Overlay, "providers")
	d, err := readOverlay(raw)
	if err != nil {
		return docPlan{}, fmt.Errorf("overlay/providers: %w", err)
	}
	if stampLogin(d, tx, reg) {
		d.changed = true
	}
	if fixTopNames(d.top) {
		d.changed = true
	}
	slices.Sort(d.oneM)
	dp := docPlan{changed: d.changed, oneM: d.oneM, write: d.changed || len(d.oneM) > 0}
	if dp.write {
		if dp.conv, err = d.render(); err != nil {
			return docPlan{}, fmt.Errorf("overlay/providers: %w", err)
		}
	}
	return dp, nil
}

// fixTopNames rewrites retired profile names in default and fallback and drops the retired profiles block.
func fixTopNames(top map[string]json.RawMessage) (changed bool) {
	if raw, ok := top["default"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && retire(s) != s {
			top["default"], _ = json.Marshal(retire(s))
			changed = true
		}
	}
	if raw, ok := top["fallback"]; ok {
		var l []string
		if json.Unmarshal(raw, &l) == nil {
			for i := range l {
				if retire(l[i]) != l[i] {
					l[i], changed = retire(l[i]), true
				}
			}
			top["fallback"], _ = json.Marshal(l)
		}
	}
	if _, ok := top["profiles"]; ok { // per-agent profile sets are retired (naming-standard D1)
		delete(top, "profiles")
		changed = true
	}
	return changed
}

// hasProfileSets reports whether the stored overlay still carries the retired profiles {active, sets} block.
func hasProfileSets(tx *vault.Tx) bool {
	old, _ := tx.Get(vault.Overlay, "providers")
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(old), &top) != nil {
		return false
	}
	_, ok := top["profiles"]
	return ok
}

// stampLogin records the GitHub login a COPILOT_PROFILE pin names as the personal row's login field (if it has none).
func stampLogin(d *legacyDoc, tx *vault.Tx, reg *providers.Registry) bool {
	login, _ := copilotLogin(tx.All(vault.Pins), reg)
	if login == "" {
		return false
	}
	idx := slices.IndexFunc(d.rows, func(r docRow) bool { return r.name == personalProfile })
	row := map[string]json.RawMessage{}
	switch {
	case idx >= 0:
		if d.rows[idx].raw == nil || json.Unmarshal(d.rows[idx].raw, &row) != nil {
			return false
		}
	default:
		b, err := json.Marshal(reg.Providers[personalProfile]) // overlay rows replace built-ins whole
		if err != nil || json.Unmarshal(b, &row) != nil {
			return false
		}
		delete(row, "provider") // implied by the nesting
	}
	if _, has := row["login"]; has {
		return false
	}
	row["login"], _ = json.Marshal(login)
	raw, err := json.Marshal(row)
	if err != nil {
		return false
	}
	if idx >= 0 {
		d.rows[idx].raw = raw
	} else {
		d.rows = append(d.rows, docRow{provider: "github", name: personalProfile, raw: raw})
	}
	d.hasRows = true
	return true
}

// Render writes the dry-run table: kind, old, new, detail.
func (p *Plan) Render(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, c := range p.Changes {
		fmt.Fprintf(tw, "%s\t%s\t->\t%s\t%s\n", c.Kind, c.Old, c.New, c.describe())
	}
	for _, c := range p.Skipped {
		fmt.Fprintf(tw, "%s\t%s\t\t\t%s\n", c.Kind, c.Old, c.Detail)
	}
	_ = tw.Flush()
}

func (c Change) describe() string {
	var parts []string
	if len(c.Sources) > 1 {
		parts = append(parts, "merges "+strings.Join(c.Sources, " + "))
	}
	if c.Len > 0 {
		parts = append(parts, fmt.Sprintf("len %d", c.Len))
	}
	if c.Keep {
		parts = append(parts, "old name kept")
	}
	if c.Detail != "" {
		parts = append(parts, c.Detail)
	}
	return strings.Join(parts, "; ")
}
