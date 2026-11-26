package route

import (
	"strings"
	"testing"
)

func TestOneOf(t *testing.T) {
	if OneOf("runner", "", Runners) != nil || OneOf("runner", RunnerPi, Runners) != nil {
		t.Fatal("empty and allowed values are valid")
	}
	if err := OneOf("runner", "x", Runners); err == nil || !strings.Contains(err.Error(), `runner: "x" is not one of pi|cli`) {
		t.Errorf("error = %v", err)
	}
}
