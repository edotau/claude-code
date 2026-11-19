package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestFile(t *testing.T) {
	value := "FAKE-SECRET-0123456789abcdef+/?\"\\"
	m, err := Load(func() ([]string, error) { return []string{value, "short123"}, nil })
	if err != nil || m.Skipped != 1 {
		t.Fatal("load counts")
	}
	forms := variants(value)
	encoded, err := json.Marshal(append(forms, "short123 password"))
	if err != nil {
		t.Fatal("fixture encoding")
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	before := append(encoded, '\n')
	if os.WriteFile(path, before, 0600) != nil {
		t.Fatal("fixture write")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("fixture stat")
	}
	hits, err := File(filepath.Dir(path), path, m, time.Hour)
	if err != nil || hits != 0 {
		t.Fatal("young file changed")
	}
	hits, err = File(filepath.Dir(path), path, m, 0)
	if err != nil || hits < len(forms)-1 {
		t.Fatalf("file hits %d", hits)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("fixture read")
	}
	if current, err := os.Stat(path); err != nil || !os.SameFile(info, current) {
		t.Fatal("inode changed")
	}
	if len(after) != len(before) || !json.Valid(after) {
		t.Fatal("file length or JSON changed")
	}
	if !bytes.Contains(after, []byte("short123 password")) {
		t.Fatal("ordinary text changed")
	}
	for i, form := range forms {
		if bytes.Contains(after, []byte(form)) {
			t.Errorf("variant %d remains", i)
		}
	}
	if hits, err = File(filepath.Dir(path), path, m, 0); err != nil || hits != 0 {
		t.Fatal("file not idempotent")
	}
}

func TestMaskCorners(t *testing.T) {
	a := "FAKE-SECRET-0123456789abcdef"
	b := a + "XYZ"
	c := a[10:] + "TAIL"
	m := FromValues([]string{a, b, c, "aaaaaaaaaaaa", "123456789012", "$FAKE_REFERENCE123", "@secret/REFERENCE123"})
	if m.Skipped != 4 {
		t.Fatal("skip count")
	}
	for i, input := range []string{a, b, a + c[len(a)-10:], "\xff" + b + "\xfe", "Bearer " + a} {
		buf := []byte(input)
		hits := m.Mask(buf)
		if hits != 1 || len(buf) != len(input) {
			t.Fatalf("case %d: hits %d length %d", i, hits, len(buf))
		}
		if i == 3 && (buf[0] != 255 || buf[len(buf)-1] != 254) {
			t.Fatal("non-match bytes changed")
		}
		saved := bytes.Clone(buf)
		if m.Mask(buf) != 0 || !bytes.Equal(buf, saved) {
			t.Fatalf("case %d: not idempotent", i)
		}
	}
	for i, form := range variants(a) {
		if out := m.String(form); len(out) != len(form) || out == form {
			t.Errorf("variant %d: unchanged or resized", i)
		}
	}
	shape := "Bearer " + strings.Repeat("Ab1_", 12)
	out := m.String(shape)
	if out == shape || m.String(out) != out {
		t.Fatal("shape coverage or idempotence")
	}
}

// A bearer token inside a JSON string stops at the closing quote, so the line stays valid JSON at the same length.
func TestMaskBearerInJSON(t *testing.T) {
	m := FromValues(nil)
	tok := strings.Repeat("Xy9", 20)
	for _, line := range []string{`{"h":"Authorization: Bearer ` + tok + `","n":1}`, `{"h":["Bearer ` + tok + `"],"n":{"a":"Bearer ` + tok + `"}}`, `{"h":"Bearer ` + tok + `\n"}`, `{"h":"Bearer=` + tok + `"}`, `{"h":"Bearer \"` + tok + `\""}`} {
		buf := []byte(line)
		if m.Mask(buf) == 0 || len(buf) != len(line) || bytes.Contains(buf, []byte(tok)) || !json.Valid(buf) {
			t.Fatalf("bearer in JSON: %q masked to %q, want same length, token gone, valid JSON", line, buf)
		}
	}
}

func TestLoadAndFileGuards(t *testing.T) {
	sentinel := errors.New("source unavailable")
	_, err := Load(func() ([]string, error) { return nil, sentinel })
	if !errors.Is(err, sentinel) || err.Error() != "redact: value source unavailable" {
		t.Fatal("source error contract")
	}
	m := FromValues([]string{`{"key":"FAKE-SECRET-0123456789abcdef","nested":["short"]}`})
	if m.Skipped != 1 || m.String("FAKE-SECRET-0123456789abcdef") == "FAKE-SECRET-0123456789abcdef" {
		t.Fatal("JSON leaves")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	input := []byte("{\"FAKE-SECRET-0123456789abcdef\": broken}\n")
	if os.WriteFile(path, input, 0600) != nil {
		t.Fatal("fixture write")
	}
	if _, err := File(dir, path, m, 0); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(input, after) {
		t.Fatal("invalid file changed")
	}
	if _, err := File(filepath.Dir(dir), dir, m, 0); err == nil {
		t.Fatal("directory accepted")
	}
}

// A truncated PEM line is masked to its body end; a line that would turn invalid stays, and the rest is still masked.
func TestFileTruncatedPEM(t *testing.T) {
	m := FromValues([]string{"FAKE-SECRET-0123456789abcdef"})
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	pem := "-----BEGIN PRI" + "VATE KEY-----\\n" + strings.Repeat("MIIEvQIBADANBgkq", 8)
	mixed := []byte("{\"k\":\"" + pem + "\"}\n{\"FAKE-SECRET-0123456789abcdef\": broken}\n")
	if os.WriteFile(path, mixed, 0600) != nil {
		t.Fatal("fixture write")
	}
	if _, err := File(dir, path, m, 0); err == nil {
		t.Fatal("invalid line skip not reported")
	}
	if got, _ := os.ReadFile(path); bytes.Contains(got, []byte("MIIEvQIBADANBgkq")) || !bytes.HasSuffix(got, mixed[bytes.LastIndexByte(mixed[:len(mixed)-1], '\n')+1:]) {
		t.Fatal("truncated PEM line not masked, or invalid line changed")
	}
}

func TestFileLinks(t *testing.T) {
	m := FromValues([]string{"FAKE-SECRET-0123456789abcdef"})
	dir := t.TempDir()
	path := filepath.Join(dir, "good.jsonl")
	if os.WriteFile(path, []byte("{\"k\":\"FAKE-SECRET-0123456789abcdef\"}\n"), 0600) != nil {
		t.Fatal("fixture write")
	}
	if os.Symlink(path, filepath.Join(dir, "link")) != nil || os.Symlink(filepath.Base(dir), dir+"-link") != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := File(dir, filepath.Join(dir, "link"), m, 0); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := File(filepath.Dir(dir), filepath.Join(dir+"-link", "good.jsonl"), m, 0); err == nil {
		t.Fatal("path through a directory symlink below the root accepted")
	}
	if hits, err := File(dir+"-link", filepath.Join(dir+"-link", "good.jsonl"), m, 0); err != nil || hits != 1 {
		t.Fatal("root reached through a symlink refused")
	}
}

// benchSample is realistic output: log-like lines with hashes, a JSON line shape, and a few planted credentials.
func benchSample(size int, jsonl bool, plant ...string) []byte {
	line := "ok  \tgithub.com/edotau/claude-code/internal/redact\t0.412s sha=3f30f220c76ffebd0 Bearer is a word, token=abc\n"
	if jsonl {
		line = `{"type":"user","message":{"content":"ok  github.com/edotau/claude-code/internal/redact 0.412s sha=3f30f220c76ffebd0 ghost eyJ"},"uuid":"0f1e2d3c"}` + "\n"
	}
	out := bytes.Repeat([]byte(line), size/len(line)+1)[:size]
	for i, p := range plant {
		copy(out[size/(len(plant)+1)*(i+1):], p)
	}
	return out
}

func BenchmarkMask(b *testing.B) {
	vals := make([]string, 200)
	for i := range vals {
		vals[i] = fmt.Sprintf("FAKE-SECRET-%04d-0123456789abcdef", i)
	}
	m := FromValues(vals)
	shapes := []string{"gh" + "p_" + strings.Repeat("Ab1", 12), "tsk" + "ey-auth-kTEST000CNTRL-" + strings.Repeat("abcde", 6), "Bearer " + strings.Repeat("Xy9", 20)}
	for _, size := range []int{64 << 10, 1 << 20, 16 << 20} {
		for _, kind := range []string{"text", "jsonl"} {
			input := benchSample(size, kind == "jsonl", append([]string{vals[199]}, shapes...)...)
			for name, mask := range map[string]func([]byte) int{"values": m.MaskValues, "all": m.Mask} {
				b.Run(fmt.Sprintf("%s/bytes=%d/%s", kind, size, name), func(b *testing.B) {
					buf := make([]byte, size)
					b.ReportAllocs()
					b.SetBytes(int64(size))
					for b.Loop() {
						copy(buf, input)
						mask(buf)
					}
				})
			}
		}
	}
}

func TestEncodedForms(t *testing.T) {
	value := "FAKE-SECRET-0123456789abcdef+/?\"\\"
	m := FromValues([]string{value})
	for i, form := range variants(value) {
		b := []byte(form)
		if hits := m.Mask(b); hits != 1 || len(b) != len(form) {
			t.Errorf("variant %d: hits %d length %d", i, hits, len(b))
		}
		if m.Mask(b) != 0 {
			t.Errorf("variant %d: not idempotent", i)
		}
	}
}

func TestHexPathPercentVariants(t *testing.T) {
	value := "Qwzrtplk mnbv+Hjgf/Ytre="
	m := FromValues([]string{value})
	for name, form := range map[string]string{
		"hex":           "51777a7274706c6b206d6e62762b486a67662f597472653d",
		"path":          "Qwzrtplk%20mnbv+Hjgf%2FYtre=",
		"percent-lower": "Qwzrtplk+mnbv%2bHjgf%2fYtre%3d",
	} {
		if b := []byte(form); m.Mask(b) != 1 || !bytes.HasPrefix(b, []byte(marker)) {
			t.Errorf("%s: form not masked", name)
		}
	}
	for _, o := range FromValues([]string{"Plmokn7Ijbuh8Ygv"}).origins { // nothing to escape: no case-folded needle
		if o.Form == "percent-lower" {
			t.Errorf("unescaped value got a percent-lower needle of length %d", o.Len)
		}
	}
}

func TestFileWindowBoundary(t *testing.T) {
	value := "FAKE-SECRET-0123456789abcdef"
	input := append([]byte("{\"content\":\""), bytes.Repeat([]byte(" "), windowSize-20)...)
	input = append(input, value...)
	input = append(input, []byte("\"}\n")...)
	path := filepath.Join(t.TempDir(), "boundary.jsonl")
	if os.WriteFile(path, input, 0600) != nil {
		t.Fatal("fixture write")
	}
	hits, err := File(filepath.Dir(path), path, FromValues([]string{value}), 0)
	if err != nil || hits != 1 {
		t.Fatalf("boundary hits %d", hits)
	}
	after, err := os.ReadFile(path)
	if err != nil || len(after) != len(input) || !json.Valid(after) || bytes.Contains(after, []byte(value)) {
		t.Fatal("boundary overwrite invariant")
	}
}
