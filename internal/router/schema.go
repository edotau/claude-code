package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/schemas"
)

// quota.json stays the bare account map on disk (v0): a still-running old daemon only reads that shape.
const schemaVersion = 0

// stateFile is the {schema,accounts} wrapper a pre-revert build wrote; readers still accept it.
type stateFile struct {
	Schema   int   `json:"schema"`
	Accounts State `json:"accounts"`
}

var format = schemas.Register(schemas.Format{
	Name: "quota", Owner: "internal/router", Kind: schemas.Derived,
	Glob:    StatePath,
	Version: schemaVersion, Keep: "bare map, no schema field: a running old daemon reads it; marks expire on their own",
})

// decodeState reads either shape; a newer-schema wrapper returns an ErrNewer error and an empty state.
func decodeState(raw []byte) (State, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return State{}, errors.New("quota: not a JSON object")
	}
	_, hasSchema := probe["schema"]
	_, hasAccounts := probe["accounts"]
	if hasSchema && hasAccounts {
		var f stateFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return State{}, err
		}
		if f.Schema > 1 {
			return State{}, fmt.Errorf("quota v%d > v1: %w", f.Schema, schemas.ErrNewer)
		}
		if f.Accounts == nil {
			f.Accounts = State{}
		}
		return f.Accounts, nil
	}
	var s State
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&s); err != nil {
		return State{}, err
	}
	if s == nil {
		s = State{}
	}
	return s, nil
}
