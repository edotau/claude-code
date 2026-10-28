package providers

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// fakeJWT is an unsigned three-part token whose payload is the given JSON claims.
func fakeJWT(claims string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(claims)) + ".sig"
}

// mintedBy is a command provider whose auth.command prints v; Refresh forces a fresh run past the cache.
func mintedBy(name, v string) *Provider {
	return &Provider{Name: name, Auth: Auth{Type: AuthKey, Command: "printf '%s' '" + v + "'"}}
}

// stateFiles lists every regular file under the state dir, so a "writes nothing" check sees any stray write.
func stateFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(paths.StateDir(), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// A fresh JWT run stores only {"exp":N} at 0600 — the claims beyond exp never reach disk.
func TestTokenExpiryWritesOnlyExp(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const exp = 1893456000
	p := mintedBy("jwt-exp", fakeJWT(`{"exp":1893456000,"sub":"secret-subject","aud":"x"}`))
	if _, err := Refresh(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.StateDir(), "cache", "token-expiry", "jwt-exp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expiry record not written: %v", err)
	}
	if want := `{"exp":1893456000}`; string(raw) != want {
		t.Errorf("record = %q, want %q", raw, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	if got, ok := TokenExpiry("jwt-exp"); !ok || got.Unix() != exp {
		t.Errorf("TokenExpiry = %v, %v; want unix %d", got, ok, exp)
	}
}

// Credentials that are not a JWT with a numeric exp leave no record behind.
func TestTokenExpiryNonJWTWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	cases := map[string]string{
		"plain":      "sk-plain-token",
		"two-parts":  "aaa.bbb",
		"no-exp":     fakeJWT(`{"sub":"x"}`),
		"string-exp": fakeJWT(`{"exp":"tomorrow"}`),
		"bad-base64": "aaa.!!!.ccc",
	}
	for name, v := range cases {
		if _, err := Refresh(ctx, mintedBy("nonjwt-"+name, v)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := TokenExpiry("nonjwt-" + name); ok {
			t.Errorf("%s: expiry recorded for a non-JWT credential", name)
		}
	}
	if files := stateFiles(t); len(files) != 0 {
		t.Errorf("non-JWT runs wrote files: %v", files)
	}
}

// A provider name that is not one safe path segment writes nothing and reads nothing.
func TestTokenExpiryUnsafeNameWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	jwt := fakeJWT(`{"exp":1893456000}`)
	for _, name := range []string{"../escape", "a/b", "..", ".", "", "sp ace", "x\x00y"} {
		p := &Provider{Name: name, Auth: Auth{Type: AuthKey, Command: "printf '%s' '" + jwt + "'"}}
		_, _ = Refresh(context.Background(), p)
		if files := stateFiles(t); len(files) != 0 {
			t.Errorf("name %q wrote %v", name, files)
		}
		if _, ok := TokenExpiry(name); ok {
			t.Errorf("name %q read an expiry", name)
		}
	}
}

// TokenExpiry is false for a missing, malformed or non-numeric record, and true only for a numeric exp.
func TestTokenExpiryReadTolerant(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, ok := TokenExpiry("never-minted"); ok {
		t.Error("missing record reported as present")
	}
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]bool{
		`{"exp":"soon"}`:     false,
		`not json`:           false,
		`{}`:                 false,
		`{"exp":1893456000}`: true,
	} {
		if err := os.WriteFile(filepath.Join(dir, "read-case.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := TokenExpiry("read-case"); ok != want {
			t.Errorf("body %s: ok=%v, want %v", body, ok, want)
		}
	}
	if _, ok := TokenExpiry("../read-case"); ok {
		t.Error("unsafe name read a record")
	}
}
