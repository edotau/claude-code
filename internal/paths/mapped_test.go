package paths

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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

func TestMappedRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{"dir": dir}
	fifo := filepath.Join(dir, "fifo")
	if err := exec.Command("mkfifo", fifo).Run(); err == nil {
		cases["fifo"] = fifo
	}
	for name, p := range cases {
		done := make(chan error, 1)
		go func() {
			_, release, err := ReadMapped(p)
			release()
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: no error", name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: ReadMapped blocked", name)
		}
	}
}

func TestMappedReleaseIdempotentAndDataMatches(t *testing.T) {
	p := writeBlob(t, 1<<14)
	want, _ := os.ReadFile(p)
	got, release, err := ReadMapped(p)
	if err != nil {
		t.Fatal(err)
	}
	copied := append([]byte(nil), got...)
	release()
	release()
	if !bytes.Equal(copied, want) {
		t.Fatal("data read before release differs from ReadFile")
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
