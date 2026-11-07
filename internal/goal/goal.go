// Package goal is the goal ledger: a tracked task with a done check, one JSON "ticket" per goal under the state dir.
package goal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// SessionEnv is the env var Claude Code sets in every live session's tools.
const SessionEnv = "CLAUDE_CODE_SESSION_ID"

// Ticket states; only Open blocks a Stop.
const (
	Open    = "open"
	Done    = "done"
	Closed  = "closed"
	Stalled = "stalled"
)

const tailBytes = 400

// MaxBlocks caps the stops one goal may block before it is declared stalled.
const MaxBlocks = 5

// IDPattern is a goal's stable task id.
var IDPattern = regexp.MustCompile(`^g-[0-9a-f]{6}$`)

// Goal is the ticket: what to finish, how to know, and the state of the last tick.
type Goal struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	Check      string `json:"check,omitempty"`      // shell command; done when it exits 0
	Run        string `json:"run,omitempty"`        // workflow run id; done when its manifest status is completed
	SessionDir string `json:"sessionDir,omitempty"` // <projects>/<slug>/<session>: holds workflows/<run>.json
	Session    string `json:"session"`
	CWD        string `json:"cwd,omitempty"`
	Plan       string `json:"plan,omitempty"` // plan file whose `(task: <id>)` line mirrors this ticket
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Created    string `json:"created"`
	Updated    string `json:"updated"`
	LastOut    string `json:"lastOut,omitempty"`
	LastHash   string `json:"lastHash,omitempty"` // hash of the last failing output; a repeat means a stall
	Blocks     int    `json:"blocks,omitempty"`   // stops this goal has blocked
}

// Dir is the ledger directory.
func Dir() string { return filepath.Join(paths.StateDir(), "goals") }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Tail keeps the last tailBytes of s, trimmed.
func Tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > tailBytes {
		s = "…" + s[len(s)-tailBytes:]
	}
	return s
}

// Hash fingerprints a check output for stall detection.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// NewID returns a fresh g-<6hex> id.
func NewID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "g-" + hex.EncodeToString(b)
}

// Line renders a goal as the tick line every surface prints: a plan checklist line carrying its task id.
func (g Goal) Line() string {
	box := " "
	if g.Status == Done {
		box = "x"
	}
	return fmt.Sprintf("- [%s] %s (task: %s)", box, g.Text, g.ID)
}

// Save writes the ticket atomically.
func Save(g Goal) error {
	if !IDPattern.MatchString(g.ID) {
		return fmt.Errorf("goal: bad id %q", g.ID)
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(filepath.Join(Dir(), g.ID+".json"), data, 0o600)
}

// Load reads one ticket.
func Load(id string) (Goal, error) {
	var g Goal
	data, err := os.ReadFile(filepath.Join(Dir(), filepath.Base(id)+".json"))
	if err != nil {
		return g, fmt.Errorf("goal %s: %w", id, err)
	}
	return g, json.Unmarshal(data, &g)
}

// All lists every readable ticket, oldest first; a bad file is skipped.
func All() []Goal {
	files, _ := filepath.Glob(filepath.Join(Dir(), "g-*.json"))
	var out []Goal
	for _, f := range files {
		if g, err := Load(strings.TrimSuffix(filepath.Base(f), ".json")); err == nil {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

// Add registers an open goal, assigning an id unless g.ID is set (adopting an existing plan line's id).
func Add(g Goal) (Goal, error) {
	if g.ID == "" {
		g.ID = NewID()
	}
	g.Status, g.Created, g.Updated = Open, now(), now()
	if g.Plan != "" {
		if err := LinkPlan(g); err != nil {
			return g, err
		}
	}
	return g, Save(g)
}

// Tick moves the ticket to status with the check output tail, then mirrors it onto the linked plan line.
func Tick(g Goal, status, reason, out string) (Goal, error) {
	g.Status, g.Reason, g.Updated, g.LastOut = status, reason, now(), Tail(out)
	if status != Open {
		g.LastHash = ""
	}
	if err := Save(g); err != nil {
		return g, err
	}
	return g, SyncPlan(g)
}

func taskLine(id string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(\s*- \[)([ xX])(\] .*\(task: ` + regexp.QuoteMeta(id) + `\).*)$`)
}

// PlanChecked reports whether the goal's plan line exists and is ticked `[x]` (a hand or commit-tick check-off).
func PlanChecked(g Goal) bool {
	data, err := os.ReadFile(g.Plan)
	if g.Plan == "" || err != nil {
		return false
	}
	m := taskLine(g.ID).FindStringSubmatch(string(data))
	return m != nil && m[2] != " "
}

// SyncPlan ticks the goal's plan line `[x]` when the goal is done; any other status leaves the plan untouched.
func SyncPlan(g Goal) error {
	if g.Plan == "" || g.Status != Done {
		return nil
	}
	return paths.WithFileLock(g.Plan, func() error {
		data, err := os.ReadFile(g.Plan)
		if err != nil {
			return err
		}
		re := taskLine(g.ID)
		if !re.Match(data) {
			return fmt.Errorf("plan %s has no line for (task: %s)", g.Plan, g.ID)
		}
		return paths.AtomicWrite(g.Plan, re.ReplaceAll(data, []byte("${1}x${3}")), 0o644)
	})
}

// LinkPlan guarantees the plan has a `(task: <id>)` line, appending one to its `## Tasks` section if absent.
func LinkPlan(g Goal) error {
	return paths.WithFileLock(g.Plan, func() error {
		data, err := os.ReadFile(g.Plan)
		if err != nil {
			return err
		}
		if taskLine(g.ID).Match(data) {
			return nil
		}
		return paths.AtomicWrite(g.Plan, []byte(insertTask(string(data), g.Line())), 0o644)
	})
}

// insertTask appends line at the end of the `## Tasks` section, creating the section when the plan has none.
func insertTask(doc, line string) string {
	start := strings.Index(doc, "\n## Tasks")
	if start < 0 {
		return strings.TrimRight(doc, "\n") + "\n\n## Tasks\n\n" + line + "\n"
	}
	end := len(doc)
	if i := strings.Index(doc[start+1:], "\n## "); i >= 0 {
		end = start + 1 + i
	}
	return strings.TrimRight(doc[:end], "\n") + "\n" + line + "\n" + doc[end:]
}
