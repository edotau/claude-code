package providers

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
)

// A user mint then an SP mint on one paired row keep separate expiry records; the status line shows the latest.
func TestSPMintKeepsUserExpiryRecord(t *testing.T) {
	p, _, _, _ := spHarness(t, "")
	userExp, spExp := time.Now().Add(2*time.Hour).Unix(), time.Now().Add(time.Hour).Unix()
	refreshU2M = func(context.Context, string, string) (oauth.Token, error) {
		return oauth.Token{Access: fakeJWT(fmt.Sprintf(`{"exp":%d}`, userExp))}, nil
	}
	stubMintM2M(t, fakeJWT(fmt.Sprintf(`{"exp":%d}`, spExp)))
	for _, as := range []string{IdentityUser, IdentitySP} {
		if _, err := Credential(WithIdentity(context.Background(), as), p); err != nil {
			t.Fatalf("%s mint: %v", as, err)
		}
	}
	if got, ok := TokenExpiry(p.Name); !ok || got.Unix() != userExp {
		t.Errorf("user expiry = %d (%v), want the user token's %d, not the SP's %d", got.Unix(), ok, userExp, spExp)
	}
	old := time.Now().Add(-time.Minute) // the user record predates the SP mint past any mtime granularity
	if err := os.Chtimes(tokenExpiryPath(p.Name), old, old); err != nil {
		t.Fatal(err)
	}
	if got, method, ok := TokenExpiryMethod(p.Name); !ok || got.Unix() != spExp || method != AuthM2M {
		t.Errorf("status line expiry = %d %q (%v), want the latest mint: SP %d m2m", got.Unix(), method, ok, spExp)
	}
}

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
	if want := `{"schema":2,"exp":1893456000,"method":"key"}`; string(raw) != want {
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
		`{"exp":"soon"}`:                false,
		`not json`:                      false,
		`{}`:                            false,
		`{"schema":1,"exp":1893456000}`: false, // a v1 record is a cache miss
		`{"schema":2,"exp":1893456000}`: true,
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

// Given an exp claim that is huge, negative or not a number, jwtExp rejects it instead of overflowing.
func TestJWTExpRejectsOutOfRange(t *testing.T) {
	for _, exp := range []string{"1e30", "-5", "9.3e18"} {
		if got, ok := JWTExp(fakeJWT(`{"exp":` + exp + `}`)); ok {
			t.Errorf("exp %s accepted as %d", exp, got)
		}
	}
	if got, ok := JWTExp(fakeJWT(`{"exp":1893456000}`)); !ok || got != 1893456000 {
		t.Errorf("valid exp = %d, %v", got, ok)
	}
}
