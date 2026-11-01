package agent

import (
	"encoding/binary"
	"testing"
)

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
