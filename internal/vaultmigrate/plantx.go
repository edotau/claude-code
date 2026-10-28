package vaultmigrate

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// pinOp is one pin rewrite: key -> newKey and/or val -> newVal; drop means newKey already holds a value.
type pinOp struct{ key, newKey, val, newVal string }

func pinOps(pins map[string]string, reg *providers.Registry) []pinOp {
	var ops []pinOp
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		nk := newPinName(k)
		nv := newPinValue(nk, pins[k])
		if _, prof := copilotLogin(map[string]string{nk: nv}, reg); prof != "" && nk == providers.PinCopilotProfile {
			nv = prof // the pin named a GitHub login, not a profile
		}
		if nk != k || nv != pins[k] {
			ops = append(ops, pinOp{k, nk, pins[k], nv})
		}
	}
	return ops
}

// secretOp is one sealed-secret rename. oauth merges refresh+access into the {refresh, access} JSON value.
type secretOp struct {
	dst                   string
	srcs                  []string
	oauth                 bool
	refreshSrc, accessSrc string
	keep                  bool
}

const (
	setupTokenName  = "CLAUDE_CODE_OAUTH_TOKEN"
	copilotTokenKey = "COPILOT_GITHUB_TOKEN"
)

func secretOps(tx *vault.Tx, reg *providers.Registry) ([]secretOp, []Change) {
	have := map[string]bool{}
	for _, n := range tx.Names(vault.Secrets) {
		have[n] = true
	}
	refs := map[string]bool{}
	for k, v := range tx.All(vault.Env) {
		if v == "@secret" {
			refs[k] = true
		}
	}
	var ops []secretOp
	var skipped []Change
	consumed := map[string]bool{}
	add := func(op secretOp, from []string) {
		if len(from) == 0 {
			return
		}
		for _, s := range from {
			consumed[s] = true
		}
		if op.dst == "" || op.dst == from[0] && len(from) == 1 {
			return
		}
		if have[op.dst] {
			skipped = append(skipped, Change{Kind: KindSkipped, Old: strings.Join(from, " + "), New: op.dst,
				Detail: "new name already exists; old left in place"})
			return
		}
		op.srcs = from
		op.keep = slices.ContainsFunc(from, func(n string) bool { return refs[n] })
		ops = append(ops, op)
	}
	existing := func(names ...string) []string {
		return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !have[n] })
	}
	for _, e := range profiles(reg) {
		p := e
		prefix := oldSecretPrefix(p.Name)
		switch {
		case p.Auth.Type == providers.AuthOAuth:
			r, a := prefix+"_REFRESH_TOKEN", prefix+"_ACCESS_TOKEN"
			add(secretOp{dst: providers.AuthSecretName(p), oauth: true, refreshSrc: r, accessSrc: a}, existing(r, a))
		case p.Auth.Type == providers.AuthM2M:
			add(secretOp{dst: providers.AuthSecretName(p)}, existing(prefix+"_CLIENT_SECRET"))
		case p.Auth.Type == providers.AuthPAT:
			add(secretOp{dst: providers.AuthSecretName(p)}, existing(prefix+"_PAT"))
		case p.Auth.Env != "":
			if dst := providers.KeySecretName(p); dst != "" {
				add(secretOp{dst: dst}, existing(p.Auth.Env))
			}
		}
	}
	for _, n := range []string{setupTokenName, copilotTokenKey} {
		if have[n] {
			skipped = append(skipped, Change{Kind: KindSkipped, Old: n, Detail: "name unchanged by the standard"})
		}
	}
	for n := range have {
		if strings.HasPrefix(n, "PROVIDER_") && !consumed[n] {
			skipped = append(skipped, Change{Kind: KindSkipped, Old: n, Detail: "no profile in the registry owns it"})
		}
	}
	slices.SortFunc(skipped, func(a, b Change) int { return strings.Compare(a.Old, b.Old) })
	return ops, skipped
}

func planTx(tx *vault.Tx, reg *providers.Registry, dp docPlan) *Plan {
	p := &Plan{}
	for _, o := range pinOps(tx.All(vault.Pins), reg) {
		c := Change{Kind: KindPinKey, Old: o.key, New: o.newKey}
		if o.key == o.newKey {
			c = Change{Kind: KindPinValue, Old: o.key + "=" + o.val, New: o.key + "=" + o.newVal}
		} else if o.val != o.newVal {
			c.Detail = fmt.Sprintf("value %s -> %s", o.val, o.newVal)
		}
		if cur, ok := tx.Get(vault.Pins, o.newKey); ok && o.newKey != o.key && cur != o.newVal {
			c.Detail = strings.TrimSpace(c.Detail + " (" + o.newKey + " already set: old value dropped)")
		}
		p.Changes = append(p.Changes, c)
	}
	ops, skipped := secretOps(tx, reg)
	for _, o := range ops {
		c := Change{Kind: KindSecret, Old: strings.Join(o.srcs, " + "), New: o.dst, Sources: o.srcs, Keep: o.keep}
		for _, s := range o.srcs {
			if r, ok, _ := tx.Record(s); ok {
				c.Len = max(c.Len, r.Len)
			}
		}
		if o.oauth {
			c.Detail = "oauth {refresh, access}"
		}
		p.Changes = append(p.Changes, c)
	}
	p.Skipped = skipped
	if dp.changed {
		p.Changes = append(p.Changes, Change{Kind: KindOverlay, Old: "overlay/providers", New: "overlay/providers",
			Detail: "flat rows -> nested by provider; retired profile names replaced"})
	}
	for _, n := range dp.oneM {
		p.Changes = append(p.Changes, Change{Kind: KindOverlay, Old: "overlay/providers." + n + ".one_m", New: "",
			Detail: "dead key dropped"})
	}
	if hasProfileSets(tx) {
		p.Changes = append(p.Changes, Change{Kind: KindOverlay, Old: "overlay/providers.profiles", New: "",
			Detail: "retired per-agent profile sets {active, sets} dropped"})
	}
	if _, ok := agentsOverlay(tx); ok {
		p.Changes = append(p.Changes, Change{Kind: KindOverlay, Old: "overlay/vendors", New: "overlay/agents",
			Detail: "key vendors -> agents"})
	}
	if _, ok := tx.Get(vault.Meta, "schema"); ok && tx.Schema() < vault.SchemaVersion {
		p.Changes = append(p.Changes, Change{Kind: KindSchema, Old: fmt.Sprint("schema ", tx.Schema()), New: fmt.Sprint("schema ", vault.SchemaVersion)})
	}
	return p
}

// oauthValue is an oauth profile's one sealed value, the shape internal/providers reads.
type oauthValue struct {
	Refresh string `json:"refresh,omitempty"`
	Access  string `json:"access,omitempty"`
}

// execute applies p's operations inside tx; k may be nil when no secret is touched.
func execute(tx *vault.Tx, reg *providers.Registry, k *vault.Key, dp docPlan) error {
	for _, o := range pinOps(tx.All(vault.Pins), reg) {
		cur, exists := tx.Get(vault.Pins, o.newKey)
		if err := tx.Put(vault.Pins, o.key, ""); err != nil {
			return err
		}
		if o.newKey != o.key && exists && cur != "" {
			continue // the new name already holds a value, which wins as it does at read time
		}
		if err := tx.Put(vault.Pins, o.newKey, o.newVal); err != nil {
			return err
		}
	}
	ops, _ := secretOps(tx, reg)
	type sealed struct {
		dst, val string
		rec      vault.Record
	}
	var writes []sealed
	remove := map[string]bool{}
	for _, o := range ops {
		vals := map[string]string{}
		var meta vault.Record
		for i, s := range o.srcs {
			v, rec, _, err := tx.OpenSecret(k, s)
			if err != nil {
				return err
			}
			vals[s] = v
			if i == 0 {
				meta = rec
			}
		}
		val := vals[o.srcs[0]]
		if o.oauth {
			b, err := json.Marshal(oauthValue{Refresh: strings.TrimSpace(vals[o.refreshSrc]), Access: strings.TrimSpace(vals[o.accessSrc])})
			if err != nil {
				return err
			}
			val, meta.Prefix = string(b), ""
		}
		writes = append(writes, sealed{o.dst, val, meta})
		if !o.keep {
			for _, s := range o.srcs {
				remove[s] = true
			}
		}
	}
	for _, w := range writes {
		if err := tx.SealSecret(k, w.dst, w.val, w.rec); err != nil {
			return err
		}
	}
	for s := range remove {
		if !slices.ContainsFunc(writes, func(w sealed) bool { return w.dst == s }) {
			if err := tx.Put(vault.Secrets, s, ""); err != nil {
				return err
			}
		}
	}
	if dp.write {
		if old, ok := tx.Get(vault.Overlay, "providers"); ok {
			if err := tx.Put(vault.Overlay, "providers.prev", old); err != nil {
				return err
			}
		}
		if err := tx.Put(vault.Overlay, "providers", dp.conv); err != nil {
			return err
		}
	}
	if doc, ok := agentsOverlay(tx); ok {
		if err := tx.Put(vault.Overlay, "agents", doc); err != nil {
			return err
		}
		if err := tx.Put(vault.Overlay, "vendors", ""); err != nil {
			return err
		}
	}
	return tx.SetSchema(vault.SchemaVersion)
}

// agentsOverlay is the old overlay/vendors document with its inner "vendors" key renamed to "agents".
// ok is false when there is nothing to move: no old overlay, or overlay/agents already exists (it wins).
func agentsOverlay(tx *vault.Tx) (string, bool) {
	old, ok := tx.Get(vault.Overlay, "vendors")
	if !ok || strings.TrimSpace(old) == "" {
		return "", false
	}
	if cur, exists := tx.Get(vault.Overlay, "agents"); exists && strings.TrimSpace(cur) != "" {
		return "", false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(old), &top) != nil {
		return old, true
	}
	if inner, has := top["vendors"]; has {
		if _, dup := top["agents"]; !dup {
			top["agents"] = inner
		}
		delete(top, "vendors")
	}
	b, err := json.Marshal(top)
	if err != nil {
		return old, true
	}
	return string(b), true
}

// profiles lists the registry's non-removed profiles in name order.
func profiles(reg *providers.Registry) []*providers.Provider {
	var out []*providers.Provider
	for _, n := range slices.Sorted(maps.Keys(reg.Providers)) {
		if p := reg.Providers[n]; p != nil {
			out = append(out, p)
		}
	}
	return out
}
