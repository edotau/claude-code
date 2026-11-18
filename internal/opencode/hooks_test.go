package opencode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestProvisionalSessionIsUniquePerDispatch pins that two dispatches in the same second never share hook state.
func TestProvisionalSessionIsUniquePerDispatch(t *testing.T) {
	a, b := provisionalSession(), provisionalSession()
	if a == b {
		t.Errorf("back-to-back provisional ids are equal (%q): concurrent dispatches would share hook state", a)
	}
}

// hostedBound keeps only registry-hosted modules whose group is on.
func TestHostedBoundFiltersByRegistrySwitch(t *testing.T) {
	vaulttest.Use(t)
	var hosted string
	for name := range hostedHooks {
		hosted = name
	}
	if hosted == "" {
		t.Fatal("the registry hosts no opencode hook module")
	}
	orig := bindHooks
	t.Cleanup(func() { bindHooks = orig })
	bindHooks = func() []hooks.Bound {
		return []hooks.Bound{
			{Group: "on", Spec: settings.EventSpec{Name: hosted}},
			{Group: "off", Spec: settings.EventSpec{Name: hosted}},
			{Group: "off-gate", Gate: true, Spec: settings.EventSpec{Name: hosted}},
			{Group: "foreign-gate", Gate: true, Spec: settings.EventSpec{Name: "not-hosted"}},
		}
	}
	var got []string
	for _, b := range hostedBound() {
		got = append(got, b.Group)
	}
	if want := []string{"on", "off", "off-gate"}; !slices.Equal(got, want) {
		t.Errorf("bound groups %v, want %v", got, want)
	}
}

// TestHookLogKeepsNewestHalfPastCap pins the 2 MiB bound: over it only whole newest lines stay, under it nothing moves.
func TestHookLogKeepsNewestHalfPastCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.log")
	var b strings.Builder
	for i := 0; b.Len() <= hookLogCap; i++ {
		fmt.Fprintf(&b, "2026-11-17T00:00:00Z s phase=before line=%07d %s\n", i, strings.Repeat("x", 40))
	}
	old := b.String()
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hookLog(path).Write([]byte("NEW\n")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	whole := strings.HasSuffix(string(got), "x\nNEW\n") && strings.HasPrefix(string(got), "2026-")
	if len(got) > hookLogCap/2+8 || !whole {
		t.Fatalf("over the cap: size %d, want <= half the cap, whole lines, newest kept", len(got))
	}
	if !strings.HasSuffix(old, strings.TrimSuffix(string(got), "NEW\n")) {
		t.Error("kept bytes are not the tail of the old log")
	}
	if _, err := hookLog(path).Write([]byte("MORE\n")); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(got)+"MORE\n" {
		t.Error("an append under the cap changed existing content")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("y", hookLogCap+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = hookLog(path).Write([]byte("LAST\n"))
	if one, _ := os.ReadFile(path); string(one) != "LAST\n" {
		t.Errorf("a window with no newline kept %d bytes, want a restart", len(one))
	}
}

func TestHookLogConcurrentAppendersAndLockTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.log")
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Go(func() {
			for i := range 50 {
				_, _ = fmt.Fprintf(hookLog(path), "w%d line %d\n", w, i)
			}
		})
	}
	wg.Wait()
	got, _ := os.ReadFile(path)
	if n := strings.Count(string(got), "\n"); n != 100 {
		t.Fatalf("lines = %d, want 100", n)
	}
	orig := hookLogLockWait
	hookLogLockWait = 50 * time.Millisecond
	t.Cleanup(func() { hookLogLockWait = orig })
	ran, err := paths.TryFileLock(path+".lock", time.Second, func() error {
		_, err := hookLog(path).Write([]byte("HELD\n"))
		return err
	})
	if after, _ := os.ReadFile(path); !ran || err != nil || !strings.HasSuffix(string(after), "HELD\n") {
		t.Fatalf("held lock: ran=%v err=%v, want the line appended after the wait", ran, err)
	}
}

// afterHost is a host whose Stop and SessionEnd hooks only count their runs.
func afterHost(seen map[string]bool) (*hookHost, *[]string) {
	var ran []string
	bind := func(event string) hooks.Bound {
		return hooks.Bound{Spec: settings.EventSpec{Name: "memory-update", Event: event, Budget: time.Second},
			Run: func(*core.Input) core.Result { ran = append(ran, event); return core.Result{} }}
	}
	h := &hookHost{bound: []hooks.Bound{bind(settings.EventStop), bind(settings.EventSessionEnd)},
		session: "s", log: io.Discard, seen: seen}
	return h, &ran
}

// TestAfterRunsMemoryUpdateOnlyWhenThereIsSomethingToSave pins an answer or a watched call as the trigger.
func TestAfterRunsMemoryUpdateOnlyWhenThereIsSomethingToSave(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seen   map[string]bool
		answer string
		status string
		err    error
		want   int
	}{
		{"tool-free answer", map[string]bool{}, "OK", wire.StatusOK, nil, 2},
		{"failed tool-free answer", map[string]bool{}, "Partial response before failure", wire.StatusError, nil, 0},
		{"stream error with answer", map[string]bool{}, "partial", wire.StatusOK, errors.New("stream failed"), 0},
		{"watched call with failure", map[string]bool{"c1": true}, "partial", wire.StatusError, errors.New("stream failed"), 2},
		{"empty answer, no calls", map[string]bool{}, "  \n", "", nil, 0},
		{"watched call, empty answer", map[string]bool{"c1": true}, "", "", nil, 2},
		{"watched call and answer", map[string]bool{"c1": true}, "done", "", nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ran := afterHost(tc.seen)
			h.after(wire.Result{Answer: tc.answer, Status: tc.status}, tc.err)
			if len(*ran) != tc.want {
				t.Errorf("hooks run = %v, want %d", *ran, tc.want)
			}
		})
	}
}

// BenchmarkAfterToolFree measures the session-end phase of a tool-free run that answered.
func BenchmarkAfterToolFree(b *testing.B) {
	h, _ := afterHost(map[string]bool{})
	for b.Loop() {
		h.after(wire.Result{Answer: "OK"}, nil)
	}
}
