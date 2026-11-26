package agentconfig

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/route"
)

func captureUnknown(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := ReportUnknownKey
	t.Cleanup(func() { ReportUnknownKey = prev })
	ReportUnknownKey = func(keys []string, origin string) { got = append(got, strings.Join(keys, ",")+"@"+origin) }
	return &got
}

func storedCtx(doc string) context.Context {
	return proc.WithValues(context.Background(), map[string]string{route.OverlayVar: doc})
}

func TestOverlayUnknownKeyTypedStops(t *testing.T) {
	got := captureUnknown(t)
	_, err := LoadOverlayContext(context.Background(), `{"bogusKey":1}`, nil)
	if err == nil || !strings.Contains(err.Error(), "bogusKey") {
		t.Fatalf("typed unknown key: err = %v, want an error naming it", err)
	}
	if len(*got) != 1 || (*got)[0] != "bogusKey@typed" {
		t.Fatalf("reported %v, want [bogusKey@typed]", *got)
	}
}

func TestOverlayUnknownKeyStoredLoads(t *testing.T) {
	got := captureUnknown(t)
	doc := `{"bogusKey":1,"bogusTwo":2,"maxChildren":"bad"}`
	if _, err := LoadOverlayContext(storedCtx(doc), "", nil); err != nil {
		t.Fatalf("stored unknown keys with a bad limit must load, got %v", err)
	}
	if want := "bogusKey,bogusTwo@env " + route.OverlayVar; len(*got) != 1 || (*got)[0] != want {
		t.Fatalf("reported %v, want one entry %q", *got, want)
	}
}

func TestOverlayUnknownKeyExplicitStored(t *testing.T) {
	captureUnknown(t)
	if _, err := LoadOverlayFrom(context.Background(), `{"bogusKey":1}`, nil, false); err != nil {
		t.Fatalf("stored flag must load an unknown key, got %v", err)
	}
}

func TestOverlayUnknownKeyNestedTerminates(t *testing.T) {
	got := captureUnknown(t)
	done := make(chan error, 1)
	go func() {
		_, err := LoadOverlayFrom(context.Background(), `{"fence":{"profiles":["dev"],"extra":1},"bogus":2}`, nil, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("nested unknown key must load, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dropStoredUnknown did not terminate")
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "extra") {
		t.Fatalf("reported %v, want one entry naming extra", *got)
	}
}

func TestCutMemberKeepsBytes(t *testing.T) {
	in := `{"provider":"anthropic", "bogus":{"a":[1,2]} ,"Provider":"databricks"}`
	out, ok := cutMember([]byte(in), "bogus")
	if want := `{"provider":"anthropic", "Provider":"databricks"}`; !ok || string(out) != want {
		t.Fatalf("cut = %q, %v; want %q", out, ok, want)
	}
	out, _ = cutMember([]byte(`{"a":1,"bogus":2}`), "bogus")
	if string(out) != `{"a":1}` {
		t.Fatalf("last member cut = %q", out)
	}
}
