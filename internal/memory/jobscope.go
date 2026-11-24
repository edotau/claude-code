package memory

import (
	"context"
	"slices"
)

// The memory job kinds JobTools knows; update also covers init and catch-up, which run through it.
const (
	JobUpdate = "update"
	JobDream  = "dream"
)

// JobTools reports whether a memory job of kind holds tools; an unknown kind gets none.
func JobTools(kind string) bool { return kind == JobUpdate || kind == JobDream }

// JobKind names the job ctx's child env marks: dream carries its own child marker, anything else is an update.
func JobKind(ctx context.Context) string { return jobKind(ChildEnv(ctx)) }

func jobKind(env []string) string {
	if slices.Contains(env, dreamChildEnv+"=1") {
		return JobDream
	}
	return JobUpdate
}
