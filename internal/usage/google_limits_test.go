package usage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gemini"
)

func TestPrintGoogleLimitsLines(t *testing.T) {
	reset := time.Date(2026, 11, 23, 3, 35, 47, 0, time.UTC)
	var buf bytes.Buffer
	printGoogleLimits(&buf, googleResult{rows: []gemini.QuotaRow{
		{Pool: "Gemini Models", Limit: "Weekly Limit Remaining", Remaining: 96, Reset: reset},
		{Pool: "Gemini 4 Argon", Limit: "Gemini 4 Argon Weekly Limit", Remaining: 0, Reset: reset},
	}})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "GOOGLE LIMITS") {
		t.Fatalf("output = %q, want a heading and one line per row", buf.String())
	}
	want := reset.Local().Format("2006-01-02 15:04")
	for _, l := range lines[1:] {
		if len([]rune(l)) > 80 || !strings.Contains(l, want) {
			t.Errorf("line %q: want at most 80 characters and the local reset %s", l, want)
		}
	}
	if !strings.Contains(lines[2], "Gemini 4 Argon · Weekly Limit") || !strings.Contains(lines[2], "0% left") {
		t.Errorf("per-model line = %q", lines[2])
	}
}

func TestPrintGoogleLimitsFailureIsOneLine(t *testing.T) {
	var buf bytes.Buffer
	printGoogleLimits(&buf, googleResult{err: errors.New("agy usage unavailable: not found")})
	if got := strings.Count(strings.TrimSpace(buf.String()), "\n"); got != 1 {
		t.Errorf("output = %q, want a heading and one error line", buf.String())
	}
	buf.Reset()
	printGoogleLimits(&buf, googleResult{})
	if buf.Len() != 0 {
		t.Errorf("an empty result printed %q, want nothing", buf.String())
	}
}

func TestRunPrintsGoogleLimitsOnlyWithQuotas(t *testing.T) {
	isolateAgentStores(t)
	orig := googleLimits
	googleLimits = func(context.Context) ([]gemini.QuotaRow, error) {
		return []gemini.QuotaRow{{Pool: "Gemini Models", Limit: "Weekly Limit Remaining", Remaining: 96}}, nil
	}
	t.Cleanup(func() { googleLimits = orig })
	for _, c := range []struct {
		args []string
		want bool
	}{{nil, true}, {[]string{"--providers=false"}, false}, {[]string{"--show", "total"}, false}} {
		var out, errw bytes.Buffer
		if code := Run(c.args, t.TempDir(), &out, &errw); code != 0 {
			t.Fatalf("Run %v = %d; stderr:\n%s", c.args, code, errw.String())
		}
		if got := strings.Contains(out.String(), "GOOGLE LIMITS"); got != c.want {
			t.Errorf("Run %v: Google section printed = %v, want %v:\n%s", c.args, got, c.want, out.String())
		}
	}
}
