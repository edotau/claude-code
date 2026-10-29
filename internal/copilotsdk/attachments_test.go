package copilotsdk

import (
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

func TestFileAttachments(t *testing.T) {
	if got := fileAttachments(nil); len(got) != 0 {
		t.Errorf("no images must give no attachments, got %v", got)
	}
	got := fileAttachments([]string{"/tmp/a/shot.png"})
	f, ok := got[0].(*copilot.AttachmentFile)
	if len(got) != 1 || !ok || f.Path != "/tmp/a/shot.png" || f.DisplayName != "shot.png" {
		t.Fatalf("attachments = %#v", got)
	}
}
