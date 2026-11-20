// Package audit records typed actions without exposing credentials.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Action is the closed set of audited events; Log drops anything else.
type Action string

const (
	ProviderSwitch    Action = "provider_switch"
	ModelPin          Action = "model_pin"
	SecretSet         Action = "secret_set"
	SecretDelete      Action = "secret_delete"
	HookBlocked       Action = "hook_blocked"
	RouterFallback    Action = "router_fallback"
	TailscaleKeyMint  Action = "tailscale_key_mint"
	TailscaleExitNode Action = "tailscale_exit_node"
	TailscaleConnect  Action = "tailscale_connect"
	TailscaleSubnet   Action = "tailscale_subnet"
	SetupAdopt        Action = "setup_adopt"
	SetupWorkspace    Action = "setup_workspace"
	CopilotAuth       Action = "copilot_auth"
	KeyRotate         Action = "key_rotate"
	KeyReveal         Action = "key_reveal"
	PlanSaved         Action = "plan_saved"
	SealFailed        Action = "seal_failed"
)

// Actions lists every valid action in report order.
var Actions = []Action{ProviderSwitch, ModelPin, SecretSet, SecretDelete, HookBlocked, RouterFallback,
	TailscaleKeyMint, TailscaleExitNode, TailscaleConnect, TailscaleSubnet, SetupAdopt, SetupWorkspace, CopilotAuth,
	KeyRotate, KeyReveal, PlanSaved, SealFailed}

func (a Action) Valid() bool {
	return slices.Contains(Actions, a)
}

type Event struct {
	Schema   int       `json:"schema"`
	Time     time.Time `json:"time"`
	Action   Action    `json:"action"`
	Resource string    `json:"resource"`
	Detail   string    `json:"detail,omitempty"`
	Session  string    `json:"session,omitempty"`
}

var maxBytes int64 = 1 << 20

// Include JSON escaping and the newline in the single-write limit.
const maxLineBytes = 4 << 10

func logFile() string { return filepath.Join(paths.StateDir(), "audit.jsonl") }

// liveLog is the log of the config dir resolved at start, before any test relocates it; a test binary never writes it.
var liveLog = logFile()

// Log appends one event. Fail-open: callers are hooks and the router hot path, so nothing is returned.
func Log(a Action, resource, detail string) { LogSession(a, "", resource, detail) }

// LogSession is Log stamped with the hook payload's session id, so a probe run (no session) is told apart.
func LogSession(a Action, session, resource, detail string) {
	if !a.Valid() {
		return
	}
	// Every credential-shaped field is redacted so the log can never become a secret store.
	event := Event{Schema: schemaVersion, Time: time.Now().UTC(), Action: a,
		Resource: textutil.RedactSecrets(resource), Detail: textutil.RedactSecrets(detail),
		Session: textutil.RedactSecrets(session)}
	var line []byte
	for {
		var err error
		line, err = json.Marshal(event)
		if err != nil {
			return
		}
		if len(line)+1 < maxLineBytes {
			break
		}
		if event.Detail == "" {
			// Drop a resource that cannot fit even without detail.
			return
		}
		event.Detail = textutil.CutBytes(event.Detail, len(event.Detail)/2)
	}
	file := logFile()
	if testing.Testing() && file == liveLog {
		return
	}
	if !prepareLogDir(filepath.Dir(file)) {
		return
	}
	// A failed rotation leaves the live file growing; appending still beats dropping the event.
	_ = rotate(file)
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_ = f.Chmod(0o600) // OpenFile's mode applies only on create
	_, _ = f.Write(append(line, '\n'))
}

func prepareLogDir(dir string) bool {
	info, err := os.Stat(dir)
	if err == nil && info.IsDir() && info.Mode() == 0o700|os.ModeDir {
		return true
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	_ = os.Chmod(dir, 0o700) // Existing directories must also have private permissions.
	return true
}

// rotate moves a full log to .1 under a lock, only after the archive shift succeeded, so a failed shift keeps history.
func rotate(file string) error {
	return paths.WithFileLock(file, func() error {
		if fi, err := os.Stat(file); err == nil && fi.Size() > maxBytes {
			if err := archiveOld(file + ".1"); err != nil {
				return err
			}
			return os.Rename(file, file+".1")
		}
		return nil
	})
}

// keepRotated bounds the archive: rotations keep .1 through .keepRotated (about keepRotated MiB) and drop the oldest.
const keepRotated = 5

// archiveOld shifts .1..(keepRotated-1) up one, the oldest falling off; a missing source is normal.
func archiveOld(rotated string) error {
	base := rotated[:len(rotated)-1]
	for n := keepRotated; n >= 2; n-- {
		err := os.Rename(base+strconv.Itoa(n-1), base+strconv.Itoa(n))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Read returns events at or after since, oldest first, across every kept archive and the live file.
func Read(since time.Time) ([]Event, error) {
	files := make([]string, 0, keepRotated+1)
	for n := keepRotated; n >= 1; n-- {
		files = append(files, logFile()+"."+strconv.Itoa(n))
	}
	files = append(files, logFile())
	var out []Event
	for _, f := range files {
		evs, err := readFile(f, since)
		out = append(out, evs...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// readFile decodes the events in one log file at or after since; a missing file yields none.
func readFile(path string, since time.Time) ([]Event, error) {
	fh, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	var out []Event
	reader := bufio.NewReader(fh)
	for {
		line, readErr := reader.ReadBytes('\n')
		var e Event
		if schemas.DecodeOK(format, line, &e) && e.Action.Valid() && !e.Time.Before(since) {
			out = append(out, e)
		}
		if readErr == io.EOF {
			return out, nil
		}
		if readErr != nil {
			return out, readErr
		}
	}
}

// realSession is a UUID (Claude Code) or a ses_ id (opencode); anything else is a test or probe stand-in.
func realSession(id string) bool {
	return strings.HasPrefix(id, "ses_") && len(id) >= 20 || uuidShape.MatchString(id)
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Synthetic counts hook_blocked events whose session id is empty or not a real session's shape.
func Synthetic(events []Event) int {
	n := 0
	for _, e := range events {
		if e.Action == HookBlocked && !realSession(e.Session) {
			n++
		}
	}
	return n
}

// Summary counts events per action.
func Summary(events []Event) map[Action]int {
	out := map[Action]int{}
	for _, e := range events {
		out[e.Action]++
	}
	return out
}

const schemaVersion = 1

var format = schemas.Register(schemas.Format{
	Name: "audit", Owner: "internal/audit", Kind: schemas.Log,
	Glob:    func() string { return logFile() + "*" },
	New:     func() any { return &Event{} },
	Version: schemaVersion, Keep: "audit trail is kept; rotated at 1 MiB", Migrate: schemas.SameShape,
})

// Report prints the typed action log (switches, pins, secret sets, safety blocks, failovers) for `doctor --audit`.
func Report(days int, format string) int {
	if days <= 0 {
		return helptext.FailUsage("doctor --audit: --days must be at least 1, got %d; try --days 7", days)
	}
	ev, err := Read(time.Now().AddDate(0, 0, -days))
	if err != nil {
		return helptext.Fail("doctor --audit: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Days      int            `json:"days"`
			Summary   map[Action]int `json:"summary"`
			Synthetic int            `json:"hook_blocked_synthetic"`
			Events    []Event        `json:"events"`
		}{days, Summary(ev), Synthetic(ev), ev}); err != nil {
			return helptext.Fail("doctor --audit: %v", err)
		}
		return 0
	}
	if len(ev) == 0 {
		fmt.Printf("no audited actions in the last %d day(s) (%s)\n", days, logFile())
		return 0
	}
	for _, e := range ev {
		fmt.Printf("%s  %-16s %-22s %s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Action, e.Resource, e.Detail)
	}
	if n := Synthetic(ev); n > 0 {
		fmt.Printf("%d of %d hook_blocked had a test/probe session id (empty or not a real session's shape)\n",
			n, Summary(ev)[HookBlocked])
	}
	return 0
}
