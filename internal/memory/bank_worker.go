package memory

import "os"

// UpdateChildEnv marks a spawned update worker so its own session hooks never schedule another.
const UpdateChildEnv = "CLAUDE_SESSION_UPDATE_CHILD"

// IsUpdateChild reports whether this process runs inside an update worker.
func IsUpdateChild() bool {
	v := os.Getenv(UpdateChildEnv)
	return v != "" && v != "0"
}
