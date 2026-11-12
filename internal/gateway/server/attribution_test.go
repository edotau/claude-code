package server

import (
	"strings"
	"testing"
)

func TestParseHeadersWarnsOnce(t *testing.T) {
	var log strings.Builder
	got := parseHeaders("A: 1\nbad name: x\nAuthorization: y\nB: v\x00\n", &log)
	if len(got) != 1 || got["A"] != "1" {
		t.Errorf("got %v", got)
	}
	if strings.Count(log.String(), "\n") != 1 {
		t.Errorf("want one warning, got %q", log.String())
	}
}
