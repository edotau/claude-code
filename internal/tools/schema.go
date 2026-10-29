package tools

import "github.com/edotau/claude-code/internal/schemas"

// schemaVersion is the agentversion cache's current schema version.
const schemaVersion = 1

// versionFile is agentversion.json on disk.
type versionFile struct {
	Schema   int               `json:"schema"`
	Versions map[string]string `json:"versions"`
}

var versionFormat = schemas.Register(schemas.Format{
	Name: "agentversion", Owner: "internal/tools", Kind: schemas.Cache,
	Glob:    versionCacheFile,
	New:     func() any { return &versionFile{} },
	Version: schemaVersion, Keep: "cache; a mismatch is a miss, rebuilt by the next --version probe",
})
