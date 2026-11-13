package gateway

import (
	"context"
	"net/http"
	"testing"
)

// Finding 5: the /d/<id> tag is added only for a daemon whose /healthz advertises it; an older one gets the bare base.
func TestTaggedBaseNeedsAdvertisingDaemon(t *testing.T) {
	older := occupy(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	current := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderDispatchTag, "1")
		_, _ = w.Write([]byte("ok\n"))
	})
	if got, want := TaggedBase(context.Background(), Base(older), "d1"), Base(older); got != want {
		t.Errorf("older daemon: %q, want the untagged %q", got, want)
	}
	if got, want := TaggedBase(context.Background(), Base(current)+"/via/h", "d1"), Base(current)+"/via/h/d/d1"; got != want {
		t.Errorf("current daemon: %q, want %q", got, want)
	}
	if got, want := TaggedBase(context.Background(), Base(1), "d1"), Base(1); got != want {
		t.Errorf("no daemon: %q, want the untagged %q", got, want)
	}
}

// UntaggedBase drops a leading /d/<id> and leaves untagged or invalid-id bases alone.
func TestUntaggedBase(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1:1/d/x-1.2/p/dev": "http://127.0.0.1:1/p/dev",
		"http://127.0.0.1:1/p/dev":         "http://127.0.0.1:1/p/dev",
		"http://127.0.0.1:1/d/a_b/p/dev":   "http://127.0.0.1:1/d/a_b/p/dev",
		"":                                 "",
	} {
		if got := UntaggedBase(raw); got != want {
			t.Errorf("UntaggedBase(%q) = %q, want %q", raw, got, want)
		}
	}
}

// DispatchBase tags a base with a valid id and leaves off one the proxy would 400.
func TestDispatchBase(t *testing.T) {
	const base = "http://127.0.0.1:1"
	for id, want := range map[string]string{
		"20261113T144909.123-codex": base + "/d/20261113T144909.123-codex",
		"":                          base,
		"a_b":                       base,
		"a/b":                       base,
	} {
		if got := DispatchBase(base, id); got != want {
			t.Errorf("DispatchBase(%q) = %q, want %q", id, got, want)
		}
	}
}
