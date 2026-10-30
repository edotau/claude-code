package databricks

import (
	"strings"
	"testing"
)

func TestRemovedFlagHint(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // substring of the error; "" means nil
	}{
		{"provider", []string{"--provider", "x"}, "--profile"},
		{"provider=", []string{"--provider=x"}, "--profile"},
		{"env", []string{"--env", "qa"}, "--profile"},
		{"tier", []string{"--tier", "opus"}, "--model"},
		{"router", []string{"--router"}, "removed"},
		{"after other args", []string{"-p", "hi", "--tier=opus"}, "--tier"},
		{"profile valid", []string{"--profile", "x"}, ""},
		{"model valid", []string{"--model=opus"}, ""},
		{"after double dash", []string{"--", "--provider"}, ""},
		{"value lookalike", []string{"--profile", "--providers"}, ""},
		{"none", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RemovedFlagHint(tt.args)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("RemovedFlagHint(%q) = %v, want nil", tt.args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RemovedFlagHint(%q) = %v, want containing %q", tt.args, err, tt.want)
			}
		})
	}
}
