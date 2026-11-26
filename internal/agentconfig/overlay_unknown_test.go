package agentconfig

import (
	"context"
	"fmt"
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
	out, dropped := cutUnknownMembers([]byte(in), true)
	if want := `{"provider":"anthropic","Provider":"databricks"}`; len(dropped) != 1 || string(out) != want {
		t.Fatalf("cut = %q, %v; want %q", out, dropped, want)
	}
	out, _ = cutUnknownMembers([]byte(`{"model":"haiku","bogus":2}`), true)
	if string(out) != `{"model":"haiku"}` {
		t.Fatalf("last member cut = %q", out)
	}
}

func TestStoredUnknownPreservesKnownMembers(t *testing.T) {
	for _, doc := range []string{
		`{"set":{"extra":"keep"},"extra":1}`,
		`{"set":{"extra":"keep"},"fence":{"extra":1}}`,
		`{"model":"haiku","fence":{"model":1},"agents":{"model":{"prompt":"keep"}}}`,
	} {
		t.Run(doc, func(t *testing.T) {
			got := captureUnknown(t)
			o, err := LoadOverlayFrom(t.Context(), doc, nil, false)
			if err != nil || len(*got) != 1 {
				t.Fatalf("load = %s, %v; reports %v", o.Doc(), err, *got)
			}
			if strings.Contains(doc, `"set"`) && o.Set["extra"] != "keep" {
				t.Errorf("valid set entry lost: %s", o.Doc())
			}
			if strings.Contains(doc, `"agents"`) && (o.Model != "haiku" || !strings.Contains(string(o.Agents), `"prompt":"keep"`)) {
				t.Errorf("valid model or agent definition lost: %s", o.Doc())
			}
		})
	}
}

func TestStoredUnknownBeyondSixtyFour(t *testing.T) {
	got := captureUnknown(t)
	var doc strings.Builder
	doc.WriteString(`{"model":"haiku"`)
	for i := range 70 {
		fmt.Fprintf(&doc, `,"extra%d":true`, i)
	}
	doc.WriteString("}")
	o, err := LoadOverlayFrom(t.Context(), doc.String(), nil, false)
	if err != nil || o.Model != "haiku" || len(*got) != 1 || strings.Count((*got)[0], "extra") != 70 {
		t.Fatalf("load = %s, %v; reports %v", o.Doc(), err, *got)
	}
}
