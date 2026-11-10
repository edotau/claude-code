package scm

import (
	"strings"
	"testing"
)

func TestDetectBitbucketContextPath(t *testing.T) {
	r, err := Detect("https://bb.example.com/bitbucket/scm/proj/repo.git", "bb.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != Bitbucket || r.Owner != "PROJ" || r.Name != "repo" {
		t.Fatalf("got %+v", r)
	}
}

func TestDetectRootPathUnchanged(t *testing.T) {
	r, err := Detect("https://bb.example.com/scm/proj/repo.git", "bb.example.com")
	if err != nil || r.Owner != "PROJ" || r.Name != "repo" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestDetectErrorRedactsCredentials(t *testing.T) {
	for _, remote := range []string{
		"https://user:ghp_SECRET@github.com/org/repo/extra",
		"https://bob:SECRET@bb.corp/scm/proj/repo/x",
	} {
		_, err := Detect(remote, "bb.corp")
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("remote %q: err = %v", remote, err)
		}
	}
}

func TestDetectSSHHostDiffersFromBase(t *testing.T) {
	r, err := Detect("ssh://git@ssh.bb.example.com:7999/proj/repo.git", "bb.example.com")
	if err != nil || r.Kind != Bitbucket || r.Owner != "PROJ" || r.Name != "repo" {
		t.Fatalf("got %+v, %v", r, err)
	}
	_, err = Detect("git@ssh.bb.example.com:proj/repo.git", "")
	if err == nil || !strings.Contains(err.Error(), "BITBUCKET_URL is unset") {
		t.Fatalf("unset base err = %v", err)
	}
}
