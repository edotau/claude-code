package paths

import "testing"

func TestWinRootFrom(t *testing.T) {
	drvfs := func(point string) []Mount { return []Mount{{Point: point, Kind: KindDrvFS, Drive: "C"}} }
	for _, tc := range []struct {
		name   string
		mounts []Mount
		want   string
	}{
		{"custom mount", drvfs("/custom/c"), "/custom/c"},
		{"no drvfs mount", nil, "/mnt/c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WinRootFrom(NewResolver(tc.mounts)); got != tc.want {
				t.Errorf("WinRootFrom = %q, want %q", got, tc.want)
			}
		})
	}
}
