package router

import (
	"context"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func geminiCells(t *testing.T, pin string) []string {
	t.Helper()
	tb := testTable(t)
	vaulttest.Use(t)
	if pin != "" {
		if err := providers.SetPins(map[string]string{providers.PinGeminiModel: pin}); err != nil {
			t.Fatal(err)
		}
	}
	r := tb.Pick(context.Background(), "sonnet", "gemini", State{}, Opts{Configured: allConfigured, Now: func() time.Time { return now0 }})
	var models []string
	for _, c := range r.Candidates {
		if c.Agent == "gemini" {
			models = append(models, c.Model)
		}
	}
	return models
}

// GEMINI_MODEL pin on the profile-less (native agy login) gemini leg beats the table cell.
func TestGeminiModelPinBeatsTableCellWithoutProfile(t *testing.T) {
	got := geminiCells(t, "gemini-pinned-x")
	if len(got) == 0 {
		t.Fatal("no gemini candidate")
	}
	for _, m := range got {
		if m != "gemini-pinned-x" {
			t.Errorf("gemini cell model = %q, want the pin", m)
		}
	}
}

// With no pin the table cell is kept.
func TestGeminiNoPinKeepsTableCell(t *testing.T) {
	got := geminiCells(t, "")
	if len(got) == 0 {
		t.Fatal("no gemini candidate")
	}
	for _, m := range got {
		if m == "" || m == "gemini-pinned-x" {
			t.Errorf("gemini cell model = %q, want the table cell", m)
		}
	}
}
