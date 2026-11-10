package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/scm"
)

func stubGH(t *testing.T, fn func(args ...string) ([]byte, error)) *[][]string {
	t.Helper()
	var calls [][]string
	orig := runGH
	runGH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return fn(args...)
	}
	origIn := runGHStdin
	runGHStdin = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return fn(args...)
	}
	t.Cleanup(func() { runGH = orig; runGHStdin = origIn })
	return &calls
}

// A PR id reads its head commit's check runs, so old gh (no `pr checks --json`) works.
func TestChecksPRReadsHeadCommitRuns(t *testing.T) {
	calls := stubGH(t, func(args ...string) ([]byte, error) {
		if args[0] == "pr" {
			return []byte(`{"number":7,"headRefOid":"abc123"}`), nil
		}
		return []byte(`{"name":"a","conclusion":"failure"}`), nil
	})
	got, err := New("o", "r").Checks(context.Background(), "7", "")
	if err != nil || len(got) != 1 || got[0].State != "fail" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if api := strings.Join((*calls)[1], " "); !strings.Contains(api, "commits/abc123/check-runs") {
		t.Fatalf("api call %q does not read the head commit", api)
	}
	for _, c := range *calls {
		if c[0] == "pr" && c[1] == "checks" {
			t.Fatalf("called gh pr checks: %v", c)
		}
	}
}

func TestChecksPRViewErrorSurfaces(t *testing.T) {
	stubGH(t, func(...string) ([]byte, error) { return nil, errors.New("boom") })
	if _, err := New("o", "r").Checks(context.Background(), "7", ""); err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}

func TestChecksShaPaginatedStream(t *testing.T) {
	calls := stubGH(t, func(...string) ([]byte, error) {
		return []byte(`{"name":"a","conclusion":"success"}` + "\n" + `{"name":"b","conclusion":"failure"}` + "\n" + `{"name":"c","conclusion":""}`), nil
	})
	got, err := New("o", "r").Checks(context.Background(), "", "abc")
	if err != nil || len(got) != 3 || got[1].State != "fail" || got[2].State != "pending" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !strings.Contains(strings.Join((*calls)[0], " "), "--paginate") {
		t.Fatalf("args %v lack --paginate", (*calls)[0])
	}
}

func TestCommentRejectsNonDigitIDs(t *testing.T) {
	calls := stubGH(t, func(...string) ([]byte, error) { return nil, nil })
	h := New("o", "r")
	for _, c := range [][2]string{{"5", "1/../../issues/9/comments"}, {"--web", ""}, {"5", "--x"}} {
		if err := h.Comment(context.Background(), c[0], "b", c[1]); err == nil {
			t.Fatalf("id %q reply %q accepted", c[0], c[1])
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("gh called: %v", *calls)
	}
}

func TestChecksRejectsBadShaAndID(t *testing.T) {
	calls := stubGH(t, func(...string) ([]byte, error) { return nil, nil })
	h := New("o", "r")
	if _, err := h.Checks(context.Background(), "", "x/../../.."); err == nil {
		t.Fatal("bad sha accepted")
	}
	if _, err := h.Checks(context.Background(), "--web", ""); err == nil {
		t.Fatal("bad id accepted")
	}
	if len(*calls) != 0 {
		t.Fatalf("gh called: %v", *calls)
	}
}

func TestListLimitZeroMeansUnlimited(t *testing.T) {
	calls := stubGH(t, func(...string) ([]byte, error) { return []byte(`[]`), nil })
	if _, err := New("o", "r").List(context.Background(), scm.ListOpts{State: "open", Limit: 0}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join((*calls)[0], " "); !strings.Contains(got, "--limit 100000") {
		t.Fatalf("args %s", got)
	}
}
