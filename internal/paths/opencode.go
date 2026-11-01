package paths

import (
	"os"
	"path/filepath"
)

// OpencodeDataDir is $XDG_DATA_HOME/opencode (default ~/.local/share/opencode).
func OpencodeDataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode")
}

// OpencodeDBPath is opencode's SQLite database (written since 1.x); "" for an in-memory DB; OPENCODE_DB overrides the stable channel default.
func OpencodeDBPath() string {
	db := os.Getenv("OPENCODE_DB")
	switch {
	case db == ":memory:":
		return ""
	case filepath.IsAbs(db):
		return db
	case db == "":
		db = "opencode.db"
	}
	return filepath.Join(OpencodeDataDir(), db)
}
