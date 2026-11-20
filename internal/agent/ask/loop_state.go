package ask

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

type loopPart struct {
	ID      string   `json:"id"`
	Prompt  string   `json:"prompt"`
	Agent   string   `json:"agent,omitempty"`
	Model   string   `json:"model,omitempty"`
	Owns    []string `json:"owns,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}

type loopPlan struct {
	Done    bool       `json:"done"`
	Summary string     `json:"summary"`
	Parts   []loopPart `json:"parts"`
}

// loopResult is one part's outcome; Status is the envelope status, or needs_main once the retry also failed.
type loopResult struct {
	Part       loopPart `json:"part"`
	Status     string   `json:"status"`
	Kind       string   `json:"kind,omitempty"`
	Ran        string   `json:"ran,omitempty"`
	Dispatches []string `json:"dispatch_ids"`
	Answer     string   `json:"answer,omitempty"`
	Retried    bool     `json:"retried,omitempty"`
	Ms         int64    `json:"duration_ms"`
}

type loopRound struct {
	N     int          `json:"n"`
	Parts []loopResult `json:"parts"`
}

// loopFile is the resumable state; Goal is stored in clear, as dispatch records store their prompts.
type loopFile struct {
	Schema   int         `json:"schema"`
	ID       string      `json:"id"`
	GoalHash string      `json:"goal_hash"`
	Goal     string      `json:"goal"`
	Rounds   []loopRound `json:"rounds"`
	Tokens   int         `json:"tokens"`
	Summary  string      `json:"summary"`
	Reason   string      `json:"reason,omitempty"`
	Updated  time.Time   `json:"updated"`
}

func loopDir() string { return filepath.Join(paths.StateDir(), "ask", "loops") }

var loopFormat = schemas.Register(schemas.Format{
	Name: "ask-loop", Owner: "internal/agent/ask", Kind: schemas.Record, Version: loopStateVersion,
	Glob: func() string { return filepath.Join(loopDir(), "*.json") },
	New:  func() any { return new(loopFile) },
	Keep: "resume state of `ask --loop`; kept until the owner deletes it",
})

func newLoopID() string {
	var b [3]byte
	_, _ = rand.Read(b[:]) // a failed read leaves zeros: the stamp alone still names the loop
	return "loop-" + time.Now().UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(b[:])
}

func newLoopFile(goal string) loopFile {
	sum := sha256.Sum256([]byte(goal))
	return loopFile{Schema: loopStateVersion, ID: newLoopID(), GoalHash: hex.EncodeToString(sum[:8]), Goal: goal}
}

// loadLoop reads a saved loop by id; the id is a file stem, never a path.
func loadLoop(id string) (loopFile, error) {
	var st loopFile
	if !batchIDRE().MatchString(id) {
		return st, fmt.Errorf("--loop-resume %q: not a loop id", id)
	}
	b, err := os.ReadFile(filepath.Join(loopDir(), id+".json"))
	if err != nil {
		return st, fmt.Errorf("--loop-resume: %w", err)
	}
	if _, err := schemas.Decode(loopFormat, b, &st); err != nil {
		return st, fmt.Errorf("--loop-resume %s: %w", id, err)
	}
	return st, nil
}

func (d *loopDriver) save() {
	d.st.Updated = time.Now().UTC()
	b, err := json.Marshal(d.st)
	if err == nil {
		if err = os.MkdirAll(loopDir(), 0o700); err == nil {
			err = paths.AtomicWrite(filepath.Join(loopDir(), d.st.ID+".json"), b, 0o600)
		}
	}
	if err != nil {
		d.logf("state not saved, the loop cannot be resumed: %v", err)
	}
}
