package dbxcfg

import (
	"encoding/json"
	"fmt"

	"github.com/edotau/claude-code/internal/providers"
)

// Row is the planned outcome for one candidate.
type Row struct {
	Candidate
	ID     string // vault provider id
	Action string // add|replace|skip
	Reason string // why skipped
}

// Options steer Plan.
type Options struct {
	Rename  map[string]string // source name to row id
	Replace bool
}

// Plan maps candidates to rows against the existing provider rows (id to raw JSON).
func Plan(cands []Candidate, existing map[string]json.RawMessage, o Options) []Row {
	rows := make([]Row, 0, len(cands))
	taken := map[string]bool{}
	names := sealedNames(existing)
	for _, c := range cands {
		r := Row{Candidate: c, ID: c.Name, Action: "add"}
		if n, ok := o.Rename[c.Name]; ok {
			r.ID = n
		}
		raw, exists := existing[r.ID]
		ek := existingAuthType(raw)
		switch {
		case c.Skip != "":
			r.Action, r.Reason = "skip", c.Skip
		case !providers.ValidID(r.ID):
			r.Action, r.Reason = "skip", "id must match [A-Za-z0-9_-]+ (use --rename)"
		case taken[r.ID]:
			r.Action, r.Reason = "skip", "duplicate id"
		case exists && !o.Replace:
			r.Action, r.Reason = "skip", "row exists (use --replace)"
		case exists && ek == "":
			r.Action, r.Reason = "skip", "existing row has no authType; switch it with `claude-code databricks login --profile "+r.ID+" --auth-type u2m`"
		case exists && ek != "" && ek != c.Auth:
			r.Action, r.Reason = "skip", "existing auth.authType "+ek+" is never changed"
		case collides(names, r):
			n := SecretNameOf(r)
			r.Action, r.Reason = "skip", "secret name "+n+" is shared with row "+names[n]+"; use --rename"
		case exists:
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
	return rows
}

// SecretNameOf is the vault name of a planned row's sealed credential, derived by providers.
func SecretNameOf(r Row) string { return providers.SecretNameFor(r.ID, r.Auth) }

// sealedNames maps each existing row's derived sealed-secret name to its id.
func sealedNames(existing map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for id, raw := range existing {
		if n := providers.SecretNameFor(id, existingAuthType(raw)); n != "" {
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

func existingAuthType(raw json.RawMessage) string {
	var p struct {
		Auth struct {
			AuthType string `json:"authType"`
		} `json:"auth"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Auth.AuthType
}

// RowJSON renders the vault row for a planned candidate.
func RowJSON(r Row) (json.RawMessage, error) {
	auth := map[string]any{"authType": r.Auth}
	if r.Auth == providers.AuthOAuthM2M {
		auth["clientId"] = r.ClientID
	}
	if r.AccountID != "" {
		auth["accountId"] = r.AccountID
	}
	if r.WorkspaceID != "" {
		auth["workspaceId"] = r.WorkspaceID
	}
	return json.Marshal(map[string]any{"preset": "databricks", "host": r.Host, "auth": auth})
}

// Apply writes the add/replace rows into the overlay doc's providers map.
func Apply(top map[string]json.RawMessage, rows []Row) error {
	provs := Existing(top)
	for _, r := range rows {
		if r.Action == "skip" {
			continue
		}
		raw, err := RowJSON(r)
		if err != nil {
			return err
		}
		provs[r.ID] = raw
	}
	b, err := json.Marshal(provs)
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
