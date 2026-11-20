package memory

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
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
	NoBash  []string // Bash patterns refused even where Bash allows them
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
		s.Write = []string{paths.MemoryStoreDir() + "/**", filepath.Join(paths.ConfigDir(), "CLAUDE.md")}
		s.Bash = append([]string{"claude-code memory review *", "claude-code memory index *",
			"claude-code memory promote *"}, planGrants(workdir)...)
		s.NoBash = []string{"claude-code plan task *-reopen*", "claude-code plan tick*",
			"claude-code memory review *-approve*", "claude-code memory review *-enforced*"} // the owner's, whatever env says
	default:
		return s
	}
	s.Tools = true
	// plan boxes change only through the plan verb, in every repo's plans home
	s.NoWrite = []string{paths.MemoryStoreDir() + "/.git/**", filepath.Join(paths.ConfigDir(), "plans") + "/**"}
	for _, w := range s.Write {
		if dir, ok := strings.CutSuffix(w, "/**"); ok {
			s.CdDirs = append(s.CdDirs, dir)
		}
	}
	if kind == JobUpdate {
		s.CdDirs = append(s.CdDirs, paths.MemoryStoreDir())
	}
	return s
}

// planGrants are dream's two plan verbs, confined to the checkout's plans home; plans.ResolvePlan is the second wall.
func planGrants(workdir string) []string {
	top := GitToplevel(workdir)
	if top == "" {
		return nil
	}
	home := plans.Home(top)
	return []string{"claude-code plan task * --done --sha * --plan " + home + "/*",
		"claude-code plan task * --strike * --plan " + home + "/*"}
}

// updateWriteScope is where an update may write: the bank's typed files and its archive/ files.
func updateWriteScope(string) []string {
	var w []string
	for _, name := range bankFiles {
		w = append(w, Path(name))
	}
	return append(w, filepath.Join(paths.MemoryStoreDir(), "archive", "*"))
}

// JobKind names the job ctx's child env marks: dream carries its own child marker, anything else is an update.
func JobKind(ctx context.Context) string { return jobKind(ChildEnv(ctx)) }

func jobKind(env []string) string {
	if slices.Contains(env, dreamChildEnv+"=1") {
		return JobDream
	}
	return JobUpdate
}
