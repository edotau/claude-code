package agent

import (
	"context"

	"bytes"
	"encoding/binary"
	"errors"
	"github.com/edotau/claude-code/internal/copilotsdk"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAttachImageDelivery(t *testing.T) {
	var pb bytes.Buffer
	if err := png.Encode(&pb, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	img, txt := filepath.Join(dir, "shot.png"), filepath.Join(dir, "note.txt")
	if err := os.WriteFile(img, pb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txt, []byte("hello-note"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := []string{img, txt}

	// codex: image rides -i right after exec; the prompt inlines only the text file.
	prompt, err := engines["codex"].attach("ask", paths, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "hello-note") || strings.Contains(prompt, "PNG") || strings.Contains(prompt, "shot.png") {
		t.Errorf("codex prompt = %q", prompt)
	}
	got := codexImageArgs([]string{"exec", "--json", "-"}, paths)
	if want := []string{"exec", "-i", img, "--json", "-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("codex argv = %v, want %v", got, want)
	}

	// claude: with tools the image is named by absolute path; without, a clear error.
	p, err := claudeAttach("ask", paths, true)
	if err != nil || !strings.Contains(p, "hello-note") || !strings.Contains(p, "Attached image (open it with the Read tool): "+img) {
		t.Errorf("claude prompt = %q, %v", p, err)
	}
	if _, err := claudeAttach("ask", paths, false); !errors.Is(err, ErrBadAttach) || !strings.Contains(err.Error(), "needs tools") {
		t.Errorf("claude without tools: %v", err)
	}

	// an image reaching the inline path is refused, never sent as bytes.
	if _, err := attachInline("ask", []string{img}); !errors.Is(err, ErrBadAttach) {
		t.Errorf("attachInline(image) = %v", err)
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

func TestCopilotAttachImageRidesSDKAttachment(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	var pb bytes.Buffer
	if err := png.Encode(&pb, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), pb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello-note"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var gotPrompt string
	var gotImages []string
	prev := copilotAsk
	copilotAsk = func(_ context.Context, _ copilotsdk.Options, prompt string, images []string) (copilotsdk.Result, error) {
		gotPrompt, gotImages = prompt, images
		return copilotsdk.Result{Text: "ok", SessionID: "s"}, nil
	}
	t.Cleanup(func() { copilotAsk = prev })
	req := RunOptions{Prompt: "p", Provider: "personal", Direct: true, Attach: []string{"shot.png", "note.txt"}}
	if _, err := (copilotRunner{}).Run(context.Background(), req, Discard); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("shot.png")
	if !reflect.DeepEqual(gotImages, []string{abs}) {
		t.Errorf("images = %v, want [%s]", gotImages, abs)
	}
	if !strings.Contains(gotPrompt, "hello-note") || strings.Contains(gotPrompt, "PNG") {
		t.Errorf("prompt = %q: want only the text file inlined", gotPrompt)
	}
}
