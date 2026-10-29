package transcript

import "github.com/edotau/claude-code/internal/schemas"

// summaryCacheSchema is the summary cache's current schema version; a mismatch is a miss, never migrated.
const summaryCacheSchema = 1

var summaryCacheFormat = schemas.Register(schemas.Format{
	Name: "transcript-summary", Owner: "internal/transcript", Kind: schemas.Cache,
	Glob:    SummaryCachePath,
	New:     func() any { return &summaryCacheFile{} },
	Version: summaryCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
