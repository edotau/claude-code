package claude

import (
	"slices"
	"strings"
	"testing"
)

func TestUltrareviewArgv(t *testing.T) {
	tests := []struct {
		name    string
		u       Ultrareview
		want    []string
		wantErr string
	}{
		{"current branch", Ultrareview{}, []string{"ultrareview"}, ""},
		{"base", Ultrareview{Base: "main"}, []string{"ultrareview", "main"}, ""},
		{"pr", Ultrareview{PR: true, Focus: "12"}, []string{"ultrareview", "12"}, ""},
		{"json", Ultrareview{Base: "main", JSON: true}, []string{"ultrareview", "main", "--json"}, ""},
		{"uncommitted", Ultrareview{Uncommitted: true}, nil, "not supported"},
		{"commit", Ultrareview{Commit: "HEAD"}, nil, "not supported"},
		{"pr without number", Ultrareview{PR: true}, nil, "PR number"},
		{"base and pr", Ultrareview{Base: "main", PR: true, Focus: "1"}, nil, "at most one"},
		{"dash ref", Ultrareview{Base: "--post"}, nil, "must not start"},
		{"focus words", Ultrareview{Focus: "auth"}, nil, "no focus"},
	}
	for _, tc := range tests {
		got, err := tc.u.Argv()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err=%v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) || slices.Contains(got, "--post") {
			t.Errorf("%s: argv=%v err=%v, want %v", tc.name, got, err, tc.want)
		}
	}
}
