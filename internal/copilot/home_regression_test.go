package copilot

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCheckNoGitHubLoginAllowsSpacedEmptyCollections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, body := range []string{`{"loggedInUsers":[ ]}`, `{"copilotTokens":{ }}`} {
		regressionWrite(t, path, body)
		if err := checkNoGitHubLogin(path); err != nil {
			t.Errorf("empty login collection %s refused: %v", body, err)
		}
	}
}

func TestRefreshLinkPreservesDestinationOnStatFailure(t *testing.T) {
	for _, name := range []string{"hooks", "permissions-config.json"} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "loop")
			if err := os.Symlink(src, src); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), name)
			if err := os.Symlink(src, dst); err != nil {
				t.Fatal(err)
			}
			if err := refreshLink(src, dst); !errors.Is(err, syscall.ELOOP) {
				t.Errorf("refreshLink err = %v; want source stat ELOOP", err)
			}
			if target, err := os.Readlink(dst); err != nil || target != src {
				t.Fatalf("destination = %q, %v; want original symlink to %s", target, err, src)
			}
		})
	}
}
