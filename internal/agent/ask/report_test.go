package ask

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
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
