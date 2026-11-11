package scm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectBitbucketContextPath(t *testing.T) {
	r, err := Detect("https://bb.example.com/bitbucket/scm/proj/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != bitbucket || r.Owner != "PROJ" || r.Name != "repo" {
		t.Fatalf("got %+v", r)
	}
}

func TestDetectRootPathUnchanged(t *testing.T) {
	r, err := Detect("https://bb.example.com/scm/proj/repo.git")
	if err != nil || r.Owner != "PROJ" || r.Name != "repo" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestDetectErrorRedactsCredentials(t *testing.T) {
	for _, remote := range []string{
		"https://user:ghp_SECRET@github.com/org/repo/extra",
		"https://bob:SECRET@bb.corp/scm/proj/repo/x",
	} {
		_, err := Detect(remote)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("remote %q: err = %v", remote, err)
		}
	}
}

func TestDetectSSHHostDiffersFromBase(t *testing.T) {
	r, err := Detect("ssh://git@ssh.bb.example.com:7999/proj/repo.git")
	if err != nil || r.Kind != bitbucket || r.Owner != "PROJ" || r.Name != "repo" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestOpenSurfacesVaultFailure(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("BITBUCKET_URL", "https://bb.example.com")
	t.Setenv("BITBUCKET_TOKEN", "")
	if err := os.MkdirAll(filepath.Join(cfg, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "state", "vault.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://bb.example.com/scm/p/r.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", a, out)
		}
	}
	t.Chdir(repo)
	_, err := Open(context.Background())
	if err == nil || strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err = %v, want the vault failure", err)
	}
}

type rewriteTransport struct {
	target string
	seen   []string
}

func (rt *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.seen = append(rt.seen, r.URL.Scheme+"://"+r.URL.Host)
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = "http", rt.target
	return http.DefaultTransport.RoundTrip(r)
}

func TestOpenDefaultsBaseToRemoteHost(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("BITBUCKET_URL", "")
	t.Setenv("BITBUCKET_TOKEN", "T")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"values":[],"isLastPage":true}`))
	}))
	defer srv.Close()
	repo := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "ssh://git@bb.example.com:7999/proj/repo.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", a, out)
		}
	}
	t.Chdir(repo)
	h, err := Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bb := h.(*BitbucketHost)
	rt := &rewriteTransport{target: strings.TrimPrefix(srv.URL, "http://")}
	bb.HTTP.Transport = rt
	if _, err := bb.List(context.Background(), ListOpts{State: "open", Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if len(rt.seen) == 0 || rt.seen[0] != "https://bb.example.com" {
		t.Fatalf("requests went to %v, want https://bb.example.com", rt.seen)
	}
}
