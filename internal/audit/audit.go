// Package audit records typed actions without exposing credentials.
package audit

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/secrets/shapes"
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
	switch a {
	case ProviderSwitch, ModelPin, SecretSet, SecretDelete, HookBlocked, RouterFallback,
		TailscaleKeyMint, TailscaleExitNode, TailscaleConnect, TailscaleSubnet, SetupAdopt, SetupNuke, SetupWorkspace, CopilotAuth,
		KeyRotate, KeyReveal, PlanSaved:
		return true
	default:
		return false
	}
}

type Event struct {
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
	event := Event{Time: time.Now().UTC(), Action: a, Resource: scrub(resource), Detail: scrub(detail)}
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
	if os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	if fi, err := os.Stat(file); err == nil && fi.Size() > maxBytes {
		_ = os.Rename(file, file+".1")
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}

// scrub replaces every credential-shaped field so the log can never become a secret store.
func scrub(s string) string { return shapes.Redact(s) }

// Read returns events at or after since, oldest first, across the rotated and live files.
func Read(since time.Time) ([]Event, error) {
	var out []Event
	for _, f := range []string{File() + ".1", File()} {
		fh, err := os.Open(f)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		reader := bufio.NewReader(fh)
		for {
			line, readErr := reader.ReadBytes('\n')
			var e Event
			if json.Unmarshal(line, &e) == nil && e.Action.Valid() && !e.Time.Before(since) {
				out = append(out, e)
			}
			if readErr != nil {
				_ = fh.Close()
				if readErr != io.EOF {
					return out, readErr
				}
				break
			}
		}
	}
	return out, nil
}

// Summary counts events per action.
func Summary(events []Event) map[Action]int {
	out := map[Action]int{}
	for _, e := range events {
		out[e.Action]++
	}
	return out
}
