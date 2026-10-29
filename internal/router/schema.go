package router

import (
	"encoding/json"

	"github.com/edotau/claude-code/internal/schemas"
)

// schemaVersion is quota.json's current schema version; v0 was the bare account map.
const schemaVersion = 1

// stateFile is quota.json on disk.
type stateFile struct {
	Schema   int   `json:"schema"`
	Accounts State `json:"accounts"`
}

var format = schemas.Register(schemas.Format{
	Name: "quota", Owner: "internal/router", Kind: schemas.Record,
	Glob:    StatePath,
	New:     func() any { return &stateFile{} },
	Version: schemaVersion, Keep: "remembered quota and auth marks expire on their own", Rewrite: true,
	Migrate: func(from int, raw []byte) ([]byte, error) { // v0 -> v1: wrap the bare map
		var accounts map[string]json.RawMessage
		if err := json.Unmarshal(raw, &accounts); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"schema": from + 1, "accounts": accounts})
	},
})
