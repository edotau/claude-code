package ask

import (
	"strings"
	"testing"
)

// Given pack flags that cannot work together or alone, askPack refuses each; valid ones build the spec the request carries.
func TestAskPackFlagCombos(t *testing.T) {
	refused := []struct {
		name string
		args []string
		want string
	}{
		{"index alone", []string{"--index"}, "--index needs"},
		{"max-files alone", []string{"--max-files", "3"}, "--max-files needs"},
		{"max-file-bytes alone", []string{"--max-file-bytes", "9"}, "--max-file-bytes needs"},
		{"negative max-files", []string{"--dirs", ".", "--max-files", "-1"}, "negative"},
		{"batch", []string{"--dirs", ".", "--batch", "-"}, "--batch"},
		{"live", []string{"--files", "*.go", "--live"}, "--live"},
		{"host", []string{"--diff", "--host", "box"}, "--host"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			fs, f := askFlagSet()
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if _, err := askPack(fs, f); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("askPack(%v) = %v, want an error containing %q", tt.args, err, tt.want)
			}
		})
	}
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--dirs", "a,b", "--index", "--max-files", "0", "--diff"}); err != nil {
		t.Fatal(err)
	}
	f.pack, _ = askPack(fs, f)
	req := (&askRun{f: f}).request("task")
	if p := req.Pack; p == nil || len(p.Dirs) != 2 || !p.Diff || !p.Index || !p.IndexOnly {
		t.Fatalf("request.Pack = %+v, want dirs a,b + diff + index + index-only", p)
	}
}
