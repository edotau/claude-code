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
	JobDigest = "digest"
)

// Scope is a memory job's tool boundary in agent-neutral form: internal/agent imports this package, so the
// agent.Permissions conversion (Edit/Write allow, NoWrite deny, Bash allow, cd rules) stays with the runner.
type Scope struct {
	Tools   bool     // false: the job's model call holds no tool at all (digest; also an unknown kind)
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
		s.Write = []string{Dir(workdir) + "/**", HomeBankDir() + "/**"}
	case JobDream:
		// dream alone may promote a gated hard rule, which it then writes into CLAUDE.md by hand
		s.Write = []string{StoreDir() + "/**", filepath.Join(paths.ConfigDir(), "CLAUDE.md")}
		s.Bash = []string{"claude-code memory timeline *", "claude-code memory review *", "claude-code memory index *",
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
	return s
}

// JobKind names the job ctx's child env marks: dream carries its own child marker, anything else is an update.
func JobKind(ctx context.Context) string { return jobKind(ChildEnv(ctx)) }

func jobKind(env []string) string {
	if slices.Contains(env, dreamChildEnv+"=1") {
		return JobDream
	}
	return JobUpdate
}
