package ask

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Text keeps ran-on and the dispatch id, json drops both, a refusal drops did-not-run.
func TestAskTrailerByFormatAndRefusal(t *testing.T) {
	ran := wire.Result{DispatchID: "d1", Agent: "claude", Status: wire.StatusOK, Hops: []wire.Hop{{Agent: "claude"}}}
	failed := wire.Result{DispatchID: "d2", Agent: "claude", Status: wire.StatusError, FallbackReason: "claude: no login"}
	if wire.RanOn(ran) == "" || wire.DidNotRun(failed) == "" {
		t.Fatal("fixtures must produce a ran-on and a did-not-run line")
	}
	for _, tc := range []struct {
		name    string
		format  string
		res     wire.Result
		refused bool
		want    []string
	}{
		{"text ran", helptext.Text, ran, false, []string{wire.RanOn(ran), "", "dispatch d1"}},
		{"json failed", helptext.JSON, failed, false, []string{"", wire.DidNotRun(failed), ""}},
		{"text refused", helptext.Text, failed, true, []string{wire.RanOn(failed), "", "dispatch d2"}},
	} {
		if got := askTrailer(tc.format, tc.res, tc.refused); !slices.Equal(got, tc.want) {
			t.Errorf("%s: askTrailer = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReportSandboxDenialsTextOnly(t *testing.T) {
	const summary = "sandbox refused 3 commands: go test, python3 -c, ps…; re-run outside the sandbox\n"
	for _, format := range []string{helptext.Text, helptext.JSON, agent.FormatStreamJSON} {
		for _, d := range []*wire.Denials{nil, {Count: 3, Commands: []string{"go test", "python3 -c", "ps aux"}}} {
			res := wire.Result{Answer: `{"ok":true}`, Structured: []byte(`{"ok":true}`), Denials: d}
			var out bytes.Buffer
			if err := reportAskResult(&out, format, res, nil); err != nil {
				t.Fatal(err)
			}
			want := "\n"
			if d != nil {
				want += summary
			}
			if format == helptext.Text {
				var saved bytes.Buffer
				if err := reportSaved(&saved, format, "", res, fmt.Errorf("run failed")); err != nil {
					t.Fatal(err)
				}
				if saved.String() != res.Answer+want {
					t.Fatalf("saved text = %q", saved.String())
				}
			}
			if format == helptext.Text && out.String() != want {
				t.Fatalf("text = %q, want %q", out.String(), want)
			}
			if format != helptext.Text && strings.Contains(out.String(), "sandbox refused") {
				t.Fatalf("summary in %s: %s", format, &out)
			}
			if res.Answer != `{"ok":true}` || string(res.Structured) != `{"ok":true}` {
				t.Fatal("answer changed")
			}
		}
	}
}

func TestPrintDenialsFitsEightyColumns(t *testing.T) {
	const tail = "; re-run outside the sandbox"
	for _, c := range []struct {
		d    wire.Denials
		want string
	}{
		{wire.Denials{Count: 1, Commands: []string{"go env"}}, "sandbox refused 1 command: go env" + tail},
		{wire.Denials{Count: 12, Commands: []string{"go test", "python3 -c", "ps aux"}}, "sandbox refused 12 commands: go test, python3 -c, p…" + tail},
		{wire.Denials{Count: 3, Commands: []string{strings.Repeat("界", 40)}}, "sandbox refused 3 commands: " + strings.Repeat("界", 11) + "…" + tail},
	} {
		var out bytes.Buffer
		if err := printDenials(&out, wire.Result{Denials: &c.d}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSuffix(out.String(), "\n"); got != c.want || textutil.DisplayWidth(got) > 80 {
			t.Errorf("summary = %q (%d cells), want %q", got, textutil.DisplayWidth(got), c.want)
		}
	}
}

func TestPrintDenialsWithoutLabels(t *testing.T) {
	var out bytes.Buffer
	if err := printDenials(&out, wire.Result{Denials: &wire.Denials{Count: 2}}); err != nil {
		t.Fatal(err)
	}
	if want := "sandbox refused 2 commands; re-run outside the sandbox\n"; out.String() != want {
		t.Errorf("summary = %q, want %q", out.String(), want)
	}
}
