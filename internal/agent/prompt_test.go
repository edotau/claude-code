package agent

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

func TestIsText(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, true},
		{"ascii", []byte("hello\n"), true},
		{"multibyte", []byte("héllo 世界 ✓"), true},
		{"nul", []byte("a\x00b"), false},
		{"invalid-utf8", []byte{0xff, 0xfe, 'a'}, false},
		{"truncated-rune", []byte{0xe4, 0xb8}, false},
	}
	for _, c := range cases {
		if got := IsText(c.in); got != c.want {
			t.Errorf("%s: IsText = %v, want %v", c.name, got, c.want)
		}
	}
}

// A binary --attach is rejected with the path named; a text file (multibyte too) is still inlined.
func TestAttachInlineRejectsBinaryKeepsText(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, data := range map[string][]byte{"nul.bin": []byte("ab\x00cd"), "bad.dat": {0xff, 0xfe, 0x80}} {
		p := write(name, data)
		_, err := AttachInline("task", []string{p})
		if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "binary") {
			t.Errorf("%s: err = %v, want a binary-file error naming %s", name, err, p)
		}
	}
	good := write("ok.txt", []byte("héllo 世界"))
	got, err := AttachInline("task", []string{good})
	if err != nil {
		t.Fatal(err)
	}
	if want := "task\n\n<file path=" + `"` + good + `"` + ">\nhéllo 世界\n</file>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIsImage(t *testing.T) {
	riff := make([]byte, 16)
	copy(riff, "RIFF")
	binary.LittleEndian.PutUint32(riff[4:], 8)
	copy(riff[8:], "WEBPVP8 ")
	for _, tc := range []struct {
		name string
		data []byte
		want bool
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), true},
		{"webp", riff, true},
		{"gif", []byte("GIF89a\x01\x00\x01\x00"), true},
		{"text", []byte("just words"), false},
		{"truncated png header", []byte("\x89PN"), false},
		{"empty", nil, false},
	} {
		if got := IsImage(tc.data); got != tc.want {
			t.Errorf("%s: IsImage = %v", tc.name, got)
		}
	}
}

func TestRubricPrompt(t *testing.T) {
	file := t.TempDir() + "/rubric.md"
	if err := writeFile(file, "RULE: no globals"); err != nil {
		t.Fatal(err)
	}
	p, err := rubricPrompt(file, "review x.go")
	if err != nil || !strings.Contains(p, "RULE: no globals") || !strings.Contains(p, "TASK: review x.go") {
		t.Fatalf("prompt=%q err=%v", p, err)
	}
	if _, err := rubricPrompt(file, strings.Repeat("x", MaxPromptBytes)); err == nil {
		t.Error("oversize prompt must be an error, never truncated")
	}
}
