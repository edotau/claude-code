package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func serveCodexRegressionRelease(t *testing.T) {
	t.Helper()
	codex := buildTarGz(t, "codex", "#!/bin/sh\nprintf 'codex 1.2.3\\n'\n")
	host := buildTarGz(t, "codex-code-mode-host", "release B host")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch r.URL.Path {
		case "/codex":
			_, err = w.Write(codex)
		case "/host":
			_, err = w.Write(host)
		default:
			_, err = fmt.Fprint(w, `{"tag_name":"rust-v1.2.3","assets":[
				{"name":"codex-x86_64-unknown-linux-musl.tar.gz","browser_download_url":"http://`+r.Host+`/codex"},
				{"name":"codex-code-mode-host-x86_64-unknown-linux-musl.tar.gz","browser_download_url":"http://`+r.Host+`/host"}]}`)
		}
		if err != nil {
			t.Errorf("serve release: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	oldURL, oldOS, oldArch := codexReleaseURL, goosOverride, goarchOverride
	codexReleaseURL, goosOverride, goarchOverride = srv.URL, "linux", "amd64"
	t.Cleanup(func() { codexReleaseURL, goosOverride, goarchOverride = oldURL, oldOS, oldArch })
}

func TestCodexRedirectCleansOriginalNpmInstall(t *testing.T) {
	root := t.TempDir()
	localBin, marker := isolateToolsEnv(t, root)
	prefix := npmToolsFixture(t, filepath.Join(root, "usr", "local"), "codex", "@openai/codex")
	setToolsPath(t, localBin, filepath.Join(prefix, "bin"))
	serveCodexRegressionRelease(t)

	var out bytes.Buffer
	if err := Install(context.Background(), &out, []string{"codex"}, false); err != nil {
		t.Fatal(err)
	}
	assertInstalledCodexBinary(t, "codex", "#!/bin/sh\nprintf 'codex 1.2.3\\n'\n")
	args, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("obsolete npm install was not cleaned up: %v\n%s", err, &out)
	}
	want := "uninstall\n-g\n--prefix\n" + mustEvalSymlinks(t, prefix) + "\n@openai/codex\n"
	if string(args) != want {
		t.Fatalf("npm arguments = %q, want %q", args, want)
	}
}

func TestBinMutationsWaitForConcurrentDirectInstall(t *testing.T) {
	for _, name := range []string{"replace", "remove", "install"} {
		t.Run(name, func(t *testing.T) {
			setupToolsTestEnv(t)
			dir := paths.BinDir()
			dst := filepath.Join(dir, "codex")
			symlinkToolsFixture(t, filepath.Join(t.TempDir(), "missing"), dst)
			done := make(chan error, 1)
			err := paths.WithFileLock(binLockPath(dir), func() error {
				started := make(chan struct{})
				go func() {
					close(started)
					done <- mutateBinForRegression(name, dst)
				}()
				<-started
				select {
				case err := <-done:
					done <- err
					t.Errorf("%s completed while the direct install held the bin lock: %v", name, err)
				case <-time.After(200 * time.Millisecond):
				}
				return paths.AtomicWrite(dst, []byte("direct install"), 0o755)
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitBinMutation(t, done)
			want := "direct install"
			if name == "install" {
				want = "next install"
			}
			assertInstalledCodexBinary(t, "codex", want)
		})
	}
}

func mutateBinForRegression(name, dst string) error {
	switch name {
	case "replace":
		kept, err := replaceLink(dst, filepath.Join(filepath.Dir(dst), "candidate"))
		if err == nil && !kept {
			return errors.New("replaceLink overwrote the concurrent direct install")
		}
		return err
	case "remove":
		status, detail := dropLink(tool{Name: "codex", Binary: "codex"}, dst, "missing", false)
		if status != "keep" {
			return fmt.Errorf("dropLink = %s %s, want keep", status, detail)
		}
		return nil
	default:
		return installBinaryLocked(io.Discard, filepath.Dir(dst), "codex", []byte("next install"))
	}
}

func awaitBinMutation(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bin mutation did not finish after the direct install released its lock")
	}
}
