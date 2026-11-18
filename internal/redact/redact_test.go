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
	hits, err := File(path, m, time.Hour)
	if err != nil || hits != 0 {
		t.Fatal("young file changed")
	}
	hits, err = File(path, m, 0)
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
	if hits, err = File(path, m, 0); err != nil || hits != 0 {
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
	if _, err := File(path, m, 0); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(input, after) {
		t.Fatal("invalid file changed")
	}
	link := filepath.Join(dir, "link")
	if os.Symlink(path, link) != nil {
		t.Fatal("fixture symlink")
	}
	if _, err := File(link, m, 0); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := File(dir, m, 0); err == nil {
		t.Fatal("directory accepted")
	}
}

func BenchmarkMask(b *testing.B) {
	vals := make([]string, 200)
	for i := range vals {
		vals[i] = fmt.Sprintf("FAKE-SECRET-%04d-0123456789abcdef", i)
	}
	m := FromValues(vals)
	for _, size := range []int{4096, 1 << 20} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			input := bytes.Repeat([]byte("ordinary prose. "), (size+15)/16)[:size]
			copy(input[size/2:], vals[199])
			buf := make([]byte, size)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				copy(buf, input)
				m.Mask(buf)
			}
		})
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

func TestFileWindowBoundary(t *testing.T) {
	value := "FAKE-SECRET-0123456789abcdef"
	input := append([]byte("{\"content\":\""), bytes.Repeat([]byte(" "), windowSize-20)...)
	input = append(input, value...)
	input = append(input, []byte("\"}\n")...)
	path := filepath.Join(t.TempDir(), "boundary.jsonl")
	if os.WriteFile(path, input, 0600) != nil {
		t.Fatal("fixture write")
	}
	hits, err := File(path, FromValues([]string{value}), 0)
	if err != nil || hits != 1 {
		t.Fatalf("boundary hits %d", hits)
	}
	after, err := os.ReadFile(path)
	if err != nil || len(after) != len(input) || !json.Valid(after) || bytes.Contains(after, []byte(value)) {
		t.Fatal("boundary overwrite invariant")
	}
}
