package memory

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// The memory job kinds JobScope knows; update also covers init and catch-up, which run through it.
const (
	JobUpdate = "update"
	JobDream  = "dream"
)

// Scope is a memory job's agent-neutral tool boundary; internal/agent imports this package and converts it.
type Scope struct {
	Tools   bool     // false: an unknown job kind holds no tools
	Write   []string // path globs Edit and Write may touch
	NoWrite []string // path globs never writable, even inside Write
	Bash    []string // Bash patterns the job may run
	CdDirs  []string // directories `cd` may enter (and below)
}

// JobScope is what a memory job of the given kind may write and run; an unknown kind gets no tools.
func JobScope(kind, workdir string) Scope {
	var s Scope
	switch kind {
	case JobUpdate:
		s.Write = updateWriteScope(workdir)
	case JobDream:
		// dream alone may promote a gated hard rule, which it then writes into CLAUDE.md by hand
		s.Write = []string{StoreDir() + "/**", filepath.Join(paths.ConfigDir(), "CLAUDE.md")}
		s.Bash = []string{"claude-code memory review --timeline *", "claude-code memory review *", "claude-code memory index *",
			"claude-code memory promote *"}
	default:
		return s
	}
	s.Tools = true
	s.NoWrite = []string{StoreDir() + "/.git/**"}
	for _, w := range s.Write {
		if dir, ok := strings.CutSuffix(w, "/**"); ok {
			s.CdDirs = append(s.CdDirs, dir)
		}
	}
	if kind == JobUpdate && storeFlat() {
		s.CdDirs = append(s.CdDirs, StoreDir(), filepath.Join(StoreDir(), "archive"))
	}
	return s
}

// updateWriteScope is where an update may write: both banks' files, never another repo's when flat.
func updateWriteScope(workdir string) []string {
	if !storeFlat() {
		return []string{BankFor(workdir).SubDir("") + "/**", HomeBank().SubDir("") + "/**"}
	}
	b := BankFor(workdir)
	var w []string
	for _, name := range bankFiles {
		w = append(w, HomeBank().Path(name))
	}
	if !b.Home() { // the prefix glob covers b's typed files too; keys never hold "--", so it cannot reach another bank
		w = append(w, filepath.Join(StoreDir(), b.Prefix()+"*.md"), filepath.Join(StoreDir(), "archive", b.Prefix()+"*"))
	}
	return w
}

// JobKind names the job ctx's child env marks: dream carries its own child marker, anything else is an update.
func JobKind(ctx context.Context) string { return jobKind(ChildEnv(ctx)) }

func jobKind(env []string) string {
	if slices.Contains(env, dreamChildEnv+"=1") {
		return JobDream
	}
	return JobUpdate
}
