package databricks

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// Row is the planned outcome for one candidate.
type Row struct {
	Candidate
	ID           string // vault provider id
	Action       string // add|replace|skip
	Reason       string // why skipped
	SPClientID   string // paired identifier, never the secret
	SPProfile    string // source section supplying the paired credential
	PairingSet   bool   // retain an explicit unpair as an empty spClientId
	identity     string
	previousHost string
	existed      bool
}

// ImportOptions steer ImportPlan.
type ImportOptions struct {
	Rename  map[string]string // source name to row id
	Replace bool
	Pair    map[string]string // SP section to user section; empty means explicitly unpaired
}

// ImportPlan maps candidates to rows against the existing provider rows (id to raw JSON).
func ImportPlan(cands []Candidate, existing map[string]json.RawMessage, o ImportOptions) []Row {
	rows := make([]Row, 0, len(cands))
	taken := map[string]bool{}
	names := sealedNames(existing)
	for _, c := range cands {
		r, ek := baseImportRow(c, existing, o)
		if reason := skipReason(r, c, ek, o, taken, names); reason != "" {
			r.Action, r.Reason = "skip", reason
		} else if r.existed {
			r.Action = "replace"
		}
		if r.Action != "skip" {
			taken[r.ID] = true
			if n := SecretNameOf(r); n != "" {
				names[n] = r.ID
			}
		}
		rows = append(rows, r)
	}
	return pairRows(rows, o.Pair, names)
}

// baseImportRow is candidate c's "add" row with the id, existing-row facts and oauth pairing carried over.
func baseImportRow(c Candidate, existing map[string]json.RawMessage, o ImportOptions) (r Row, ek string) {
	r = Row{Candidate: c, ID: c.Name, Action: "add"}
	if n, ok := o.Rename[c.Name]; ok {
		r.ID = n
	}
	raw, exists := existing[r.ID]
	ek = existingAuthType(raw)
	r.existed, r.previousHost = exists, existingHost(raw)
	if ek == providers.AuthOAuth {
		var p struct {
			Auth     map[string]json.RawMessage `json:"auth"`
			Identity string                     `json:"identity"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return r, "" // a malformed row reads as unreadable, so the import skips it
		}
		value, set := p.Auth["spClientId"]
		if set && json.Unmarshal(value, &r.SPClientID) != nil {
			return r, ""
		}
		r.PairingSet, r.identity = set, p.Identity
	}
	return r, ek
}

// skipReason is why row r is not imported ("" = importable); the checks run in a fixed order.
func skipReason(r Row, c Candidate, ek string, o ImportOptions, taken map[string]bool, names map[string]string) string {
	switch {
	case c.Skip != "":
		return c.Skip
	case !providers.ValidID(r.ID):
		return "id must match [A-Za-z0-9_-]+ (use --rename)"
	case taken[r.ID]:
		return "duplicate id"
	}
	if r.existed {
		if reason := existingSkipReason(r, c, ek, o); reason != "" {
			return reason
		}
	}
	if r.SPClientID != "" && r.previousHost != c.Host {
		return "paired profile host is never changed on re-import"
	}
	if collides(names, r) {
		n := SecretNameOf(r)
		return "secret name " + n + " is shared with row " + names[n] + "; use --rename"
	}
	return ""
}

// existingSkipReason is why an already-present row is left alone ("" = it may be replaced).
func existingSkipReason(r Row, c Candidate, ek string, o ImportOptions) string {
	switch {
	case !o.Replace:
		return "row exists (use --replace)"
	case ek == "":
		return "existing row has no readable sealed auth.type; switch it with `claude-code databricks login --profile " + r.ID + " --auth-type oauth`"
	case ek != c.Auth:
		return "existing auth.type " + ek + " is never changed"
	}
	return ""
}

// SecretNameOf is the vault name of a planned row's sealed credential, derived by providers.
func SecretNameOf(r Row) string {
	if r.Auth == providers.AuthOAuth && r.SPClientID != "" {
		return secretName(r.ID, providers.AuthM2M)
	}
	return secretName(r.ID, r.Auth)
}

// secretName is the vault name of a databricks profile's sealed secret for an auth type.
func secretName(id, authType string) string {
	return providers.SecretName(providers.GroupDatabricks, id, authType)
}

// sealedNames maps each existing row's derived sealed-secret name to its id.
func sealedNames(existing map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for id, raw := range existing {
		var p struct {
			Auth struct {
				SPClientID string `json:"spClientId"`
			} `json:"auth"`
		}
		_ = json.Unmarshal(raw, &p) // an unrelated bad field must not hide the row's sealed secret
		auth := existingAuthType(raw)
		if auth == providers.AuthOAuth && p.Auth.SPClientID != "" {
			out[secretName(id, providers.AuthM2M)] = id
		}
		if n := secretName(id, auth); n != "" {
			out[n] = id
		}
	}
	return out
}

// collides reports whether r's secret name already belongs to a different row.
func collides(names map[string]string, r Row) bool {
	n := SecretNameOf(r)
	o, ok := names[n]
	return n != "" && ok && o != r.ID
}

// existingAuthType is an existing row's sealed auth.type (oauth, m2m, pat); "" for any other row.
func existingAuthType(raw json.RawMessage) string {
	var p struct {
		Auth struct {
			Type string `json:"type"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	if !slices.Contains([]string{providers.AuthOAuth, providers.AuthM2M, providers.AuthPAT}, p.Auth.Type) {
		return ""
	}
	return p.Auth.Type
}

// RowJSON renders the vault row for a planned candidate.
func RowJSON(r Row) (json.RawMessage, error) {
	auth := map[string]any{"type": r.Auth}
	if r.Auth == providers.AuthM2M {
		auth["clientId"] = r.ClientID
	}
	if r.AccountID != "" {
		auth["accountId"] = r.AccountID
	}
	if r.WorkspaceID != "" {
		auth["workspaceId"] = r.WorkspaceID
	}
	if r.PairingSet && r.Auth == providers.AuthOAuth {
		auth["spClientId"] = r.SPClientID
	}
	out := map[string]any{"preset": "databricks", "host": r.Host, "auth": auth}
	if r.identity != "" {
		out["identity"] = r.identity
	}
	return json.Marshal(out)
}

// Apply writes the add/replace rows into the overlay doc's providers map.
func Apply(top map[string]json.RawMessage, rows []Row) error {
	if raw, ok := top["providers"]; ok {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("providers overlay: existing value is not an object: %w", err)
		}
	}
	provs := Existing(top)
	keys := providers.ObjectKeys(top["providers"])
	for _, r := range rows {
		if r.Action == "skip" {
			continue
		}
		raw, err := RowJSON(r)
		if err != nil {
			return err
		}
		if _, ok := provs[r.ID]; !ok {
			keys = append(keys, r.ID)
		}
		provs[r.ID] = raw
	}
	b, err := providers.MarshalOrdered(keys, provs)
	if err != nil {
		return fmt.Errorf("providers overlay: %w", err)
	}
	top["providers"] = b
	return nil
}

// Existing returns the overlay's provider rows by id.
func Existing(top map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if raw, ok := top["providers"]; ok {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// pairRows consumes explicit choices before same-host inference; any existing user pairing decision is final.
func pairRows(rows []Row, choices map[string]string, names map[string]string) []Row {
	for _, i := range pairOrder(rows, choices) {
		sp := &rows[i]
		if sp.Auth != providers.AuthM2M || sp.Skip != "" {
			continue
		}
		users, countSP := sameHostRows(rows, *sp)
		target, explicit := pairTarget(rows, *sp, users, countSP, choices)
		paired := attachToTarget(rows, users, target, sp, names)
		if !paired && len(users) > 0 && !explicit && sp.Action != "skip" {
			sp.Reason = "unpaired; choose a service principal in configure"
		}
	}

	markDeclinedPairings(rows, choices)
	return rows
}

// attachToTarget attaches sp to the same-host user row named target, reporting whether one took it.
func attachToTarget(rows []Row, users []int, target string, sp *Row, names map[string]string) bool {
	for _, j := range users {
		if rows[j].Name == target && attachSP(&rows[j], sp, names) {
			return true
		}
	}
	return false
}

// pairOrder is rows' indexes with explicit choices and saved pairings first, otherwise stable.
func pairOrder(rows []Row, choices map[string]string) []int {
	order := make([]int, len(rows))
	for i := range rows {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		_, a := choices[rows[i].Name]
		_, b := choices[rows[j].Name]
		a, b = a || rows[i].PairSet, b || rows[j].PairSet
		if a == b {
			return 0
		}
		if a {
			return -1
		}
		return 1
	})
	return order
}

// markDeclinedPairings records an explicit "no pairing" on the same-host users still without a service principal.
func markDeclinedPairings(rows []Row, choices map[string]string) {
	for _, sp := range rows {
		target, explicit := choices[sp.Name]
		if !explicit {
			target, explicit = sp.Pair, sp.PairSet
		}
		if sp.Auth != providers.AuthM2M || !explicit || target != "" {
			continue
		}
		for j := range rows {
			u := &rows[j]
			if u.Auth == providers.AuthOAuth && u.Host == sp.Host && u.SPClientID == "" {
				u.PairingSet = true
			}
		}
	}
}

// pairingTags are a candidate's Env plus its name tokens; they only order choices, never override host safety.
func pairingTags(c Candidate) []string {
	tags := strings.FieldsFunc(strings.ToLower(c.Name), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	if c.Env != "" {
		tags = append(tags, strings.ToLower(c.Env))
	}
	return tags
}

// existingHost reads a row without expanding or redacting it.
func existingHost(raw json.RawMessage) string {
	var p providers.Provider
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	host, _ := normalizeHost(providers.AuthHost(&p))
	return host
}

// sameHostRows counts source profiles, including ones that an earlier pairing already consumed.
func sameHostRows(rows []Row, sp Row) (users []int, principals int) {
	for j, u := range rows {
		if u.Skip != "" || u.Host != sp.Host {
			continue
		}
		if u.Auth == providers.AuthM2M {
			principals++
		}
		if u.Auth == providers.AuthOAuth {
			users = append(users, j)
		}
	}
	return users, principals
}

func pairTarget(rows []Row, sp Row, users []int, principals int, choices map[string]string) (string, bool) {
	if target, ok := choices[sp.Name]; ok {
		return target, true
	}
	if sp.PairSet {
		return sp.Pair, true
	}
	for _, j := range users {
		if rows[j].SPClientID == sp.ClientID && rows[j].previousHost == sp.Host {
			return rows[j].Name, false
		}
	}
	if len(users) == 1 && principals == 1 {
		return rows[users[0]].Name, false
	}
	return "", false
}

// canAttachSP is the row-state half of attachSP's guard: no stored decision or foreign credential is overridden.
func canAttachSP(u, sp *Row) bool {
	if u.Action == "skip" && (!u.existed || u.SPClientID != sp.ClientID) {
		return false
	}
	if u.PairingSet && (u.SPClientID == "" || u.SPClientID != sp.ClientID || u.previousHost != sp.Host) {
		return false
	}
	return u.SPProfile == "" && (u.Action == "skip" || sp.Action != "skip")
}

// attachSP never replaces a stored pairing decision or borrows a credential from a different host.
func attachSP(u, sp *Row, names map[string]string) bool {
	if !canAttachSP(u, sp) {
		return false
	}
	name := secretName(u.ID, providers.AuthM2M)
	if owner := names[name]; owner != "" && owner != u.ID {
		return false
	}
	u.SPClientID, u.SPProfile, u.PairingSet, u.HasSecret = sp.ClientID, sp.Name, true, sp.HasSecret
	names[name] = u.ID
	sp.Action, sp.Reason = "skip", "paired with "+u.ID
	return true
}
