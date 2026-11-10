// Package audit records typed actions without exposing credentials.
package audit

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
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
	SetupNuke         Action = "setup_nuke"
	SetupWorkspace    Action = "setup_workspace"
	CopilotAuth       Action = "copilot_auth"
	KeyRotate         Action = "key_rotate"
	KeyReveal         Action = "key_reveal"
	PlanSaved         Action = "plan_saved"
)

// Actions lists every valid action in report order.
var Actions = []Action{ProviderSwitch, ModelPin, SecretSet, SecretDelete, HookBlocked, RouterFallback,
	TailscaleKeyMint, TailscaleExitNode, TailscaleConnect, TailscaleSubnet, SetupAdopt, SetupNuke, SetupWorkspace, CopilotAuth,
	KeyRotate, KeyReveal, PlanSaved}

func (a Action) Valid() bool {
	return slices.Contains(Actions, a)
}

type Event struct {
	Schema   int       `json:"schema"`
	Time     time.Time `json:"time"`
	Action   Action    `json:"action"`
	Resource string    `json:"resource"`
	Detail   string    `json:"detail,omitempty"`
}

// maxBytes rotates the log to audit.jsonl.1; a var so tests can shrink it.
var maxBytes int64 = 1 << 20

// Include JSON escaping and the newline in the single-write limit.
const maxLineBytes = 4 << 10

// File is the live log.
func File() string { return filepath.Join(paths.StateDir(), "audit.jsonl") }

// Log appends one event. Fail-open: callers are hooks and the router hot path, so nothing is returned.
func Log(a Action, resource, detail string) {
	if !a.Valid() {
		return
	}
	event := Event{Schema: schemaVersion, Time: time.Now().UTC(), Action: a, Resource: scrub(resource), Detail: scrub(detail)}
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
		n := len(event.Detail) / 2
		for n > 0 && !utf8.RuneStart(event.Detail[n]) {
			n--
		}
		event.Detail = event.Detail[:n]
	}
	file := File()
	dir := filepath.Dir(file)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_ = os.Chmod(dir, 0o700) // MkdirAll's mode applies only on create
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

// rotate moves a full log to .1 under a lock, re-checking the size so racing writers rotate once.
// It renames over .1 only after the archive shift succeeded, so a failed shift never drops history.
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

// archiveOld shifts .1..(keepRotated-1) up one so a new rotation never overwrites history; the oldest falls off.
// A missing source is normal before the archive fills; any other rename failure is returned.
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

// scrub replaces every credential-shaped field so the log can never become a secret store.
func scrub(s string) string { return textutil.RedactSecrets(s) }

// Read returns events at or after since, oldest first, across every kept archive and the live file.
func Read(since time.Time) ([]Event, error) {
	files := make([]string, 0, keepRotated+1)
	for n := keepRotated; n >= 1; n-- {
		files = append(files, File()+"."+strconv.Itoa(n))
	}
	files = append(files, File())
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

// Summary counts events per action.
func Summary(events []Event) map[Action]int {
	out := map[Action]int{}
	for _, e := range events {
		out[e.Action]++
	}
	return out
}

// schemaVersion is the audit log's current schema version.
const schemaVersion = 1

var format = schemas.Register(schemas.Format{
	Name: "audit", Owner: "internal/audit", Kind: schemas.Log,
	Glob:    func() string { return File() + "*" },
	New:     func() any { return &Event{} },
	Version: schemaVersion, Keep: "audit trail is kept; rotated at 1 MiB", Migrate: schemas.SameShape,
})
