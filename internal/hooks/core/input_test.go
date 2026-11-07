package core

import (
	"strings"
	"testing"
)

func TestParseInputStewardFields(t *testing.T) {
	in := ParseInput(strings.NewReader(`{"background_tasks":[{"id":"a"},{"id":"b"}],"trigger":"auto"}`))
	if in.BackgroundTasks != 2 || in.Trigger != "auto" {
		t.Fatalf("BackgroundTasks=%d Trigger=%q", in.BackgroundTasks, in.Trigger)
	}
}
