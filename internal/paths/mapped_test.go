package paths

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeBlob(tb testing.TB, size int) string {
	tb.Helper()
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*31 + i>>8)
	}
	p := filepath.Join(tb.TempDir(), "blob")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		tb.Fatal(err)
	}
	return p
}

func TestReadMappedMatchesReadFile(t *testing.T) {
	p := writeBlob(t, 1<<20)
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got, release, err := ReadMapped(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("mapped bytes differ from ReadFile")
	}
	release()
	release()
}

func TestMappedEmptyAndMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, release, err := ReadMapped(p)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: len=%d err=%v", len(got), err)
	}
	release()
	_, release, err = ReadMapped(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("missing file returned no error")
	}
	release()
}

func TestMappedSurvivesRenameReplace(t *testing.T) {
	p := writeBlob(t, 1<<16)
	want, _ := os.ReadFile(p)
	got, release, err := ReadMapped(p)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	tmp := p + ".new"
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("mapping changed after rename replace")
	}
}

func BenchmarkMappedVsReadFile(b *testing.B) {
	p := writeBlob(b, 17<<20)
	b.Run("ReadFile", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			d, err := os.ReadFile(p)
			if err != nil || len(d) == 0 {
				b.Fatal(err)
			}
		}
	})
	b.Run("Mapped", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			d, release, err := ReadMapped(p)
			if err != nil || len(d) == 0 {
				b.Fatal(err)
			}
			release()
		}
	})
}
