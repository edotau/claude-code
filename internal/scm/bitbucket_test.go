package scm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing" // tests refuse real DNS: mock API calls

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestDiffTruncatedBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("diff --git a/x"))
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()
	h := NewBitbucket(srv.URL, "P", "r", "TOKEN")
	d, err := h.Diff(context.Background(), "1")
	if err == nil {
		t.Fatalf("truncated diff returned as success: %q", d)
	}
}

func TestDiffCompleteBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "diff --git a/x") }))
	defer srv.Close()
	d, err := NewBitbucket(srv.URL, "P", "r", "T").Diff(context.Background(), "1")
	if err != nil || d != "diff --git a/x" {
		t.Fatalf("%q, %v", d, err)
	}
}

func listServer(t *testing.T, pages int) (*BitbucketHost, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var start int
		fmt.Sscan(r.URL.Query().Get("start"), &start)
		last := start+1 >= pages
		fmt.Fprintf(w, `{"values":[{"id":%d,"fromRef":{},"toRef":{}},{"id":%d,"fromRef":{},"toRef":{}}],"isLastPage":%t,"nextPageStart":%d}`,
			start*2+1, start*2+2, last, start+1)
	}))
	t.Cleanup(srv.Close)
	return NewBitbucket(srv.URL, "P", "r", "T"), &hits
}

func TestListLimitStopsFetchingPages(t *testing.T) {
	h, hits := listServer(t, 5)
	prs, err := h.List(context.Background(), ListOpts{State: "open", Limit: 3})
	if err != nil || len(prs) != 3 || prs[2].ID != "3" {
		t.Fatalf("%+v, %v", prs, err)
	}
	if *hits != 2 {
		t.Fatalf("fetched %d pages, want 2", *hits)
	}
}

func TestListAuthorFilterStillReadsAllPages(t *testing.T) {
	h, hits := listServer(t, 3)
	prs, err := h.List(context.Background(), ListOpts{State: "open", Author: "nobody", Limit: 1})
	if err != nil || len(prs) != 0 || *hits != 3 {
		t.Fatalf("%+v, %v, hits %d", prs, err, *hits)
	}
}

func TestChecksReadsEveryPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") == "0" {
			fmt.Fprint(w, `{"values":[{"state":"SUCCESSFUL","name":"a"}],"isLastPage":false,"nextPageStart":1}`)
			return
		}
		fmt.Fprint(w, `{"values":[{"state":"FAILED","name":"b"}],"isLastPage":true}`)
	}))
	defer srv.Close()
	got, err := NewBitbucket(srv.URL, "P", "r", "T").Checks(context.Background(), "", "abc")
	if err != nil || len(got) != 2 || got[1].Name != "b" || got[1].State != "fail" {
		t.Fatalf("%+v, %v", got, err)
	}
}

func emptyPageHost(t *testing.T) *BitbucketHost {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"values":[],"isLastPage":true}`)
	}))
	t.Cleanup(srv.Close)
	return NewBitbucket(srv.URL, "P", "r", "T")
}

func TestListEmptyIsNonNilSlice(t *testing.T) {
	prs, err := emptyPageHost(t).List(context.Background(), ListOpts{State: "open"})
	if err != nil || prs == nil {
		t.Fatalf("%#v, %v: want empty non-nil slice", prs, err)
	}
}

func TestChecksEmptyIsNonNilSlice(t *testing.T) {
	got, err := emptyPageHost(t).Checks(context.Background(), "", "abc")
	if err != nil || got == nil {
		t.Fatalf("%#v, %v: want empty non-nil slice", got, err)
	}
}

func TestForbiddenDoesNotBlameToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	_, err := NewBitbucket(srv.URL, "P", "r", "T").View(context.Background(), "1")
	if err == nil || strings.Contains(err.Error(), "vault set") || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("403 error = %v, want a permission hint without the vault fix", err)
	}
}

func TestInvalidIDsNeverReachServer(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	h, ctx := NewBitbucket(srv.URL, "P", "r", "T"), context.Background()
	_, verr := h.View(ctx, "..")
	_, derr := h.Diff(ctx, "../x")
	cerr := h.Comment(ctx, "..", "hi", "")
	rerr := h.Comment(ctx, "1", "hi", "abc")
	if verr == nil || derr == nil || cerr == nil || rerr == nil || hits != 0 {
		t.Fatalf("errs %v %v %v %v, hits %d", verr, derr, cerr, rerr, hits)
	}
}

func TestCommentReplyToIsNumericParent(t *testing.T) {
	var got struct{ Parent struct{ ID any } }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := NewBitbucket(srv.URL, "P", "r", "T").Comment(context.Background(), "1", "hi", "42"); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Parent.ID.(float64); !ok || got.Parent.ID != float64(42) {
		t.Fatalf("parent id = %#v, want JSON number 42", got.Parent.ID)
	}
}

func TestChecksEmptyHeadCommitIsClearError(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		fmt.Fprint(w, `{"id":7,"fromRef":{},"toRef":{}}`)
	}))
	defer srv.Close()
	_, err := NewBitbucket(srv.URL, "P", "r", "T").Checks(context.Background(), "7", "")
	if err == nil || !strings.Contains(err.Error(), "no head commit") || hits != 1 {
		t.Fatalf("err %v, hits %d: want 'no head commit' after only the PR fetch", err, hits)
	}
}

func TestCreateDetachedHeadIsClearError(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"}, {"checkout", "-q", "--detach"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(dir)
	_, err := NewBitbucket("http://127.0.0.1:1", "P", "r", "T").Create(context.Background(), CreateOpts{Title: "t", Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("err = %v, want detached HEAD error", err)
	}
}
