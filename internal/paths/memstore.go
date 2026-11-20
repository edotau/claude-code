package paths

import "path/filepath"

// SessionIDEnv is the env var Claude Code sets in every live session's tools.
const SessionIDEnv = "CLAUDE_CODE_SESSION_ID"

// MemoryStoreDir is the memory store root and the work tree of the local memory repo.
func MemoryStoreDir() string { return filepath.Join(ConfigDir(), "memory") }

// MemoryStateDir holds update stamps, results, logs and the session chain.
func MemoryStateDir() string { return filepath.Join(StateDir(), "memory") }

// MemorySubDir is the store root: a named sub is a filename prefix, not a directory.
func MemorySubDir(sub string) string { return filepath.Join(MemoryStoreDir(), sub) }

// MemorySub is name inside the store folder sub (archive, handoffs, reports, …); sub "" is a top-level file.
func MemorySub(sub, name string) string {
	return filepath.Join(MemoryStoreDir(), sub, filepath.FromSlash(name))
}
