package clean

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Nested is <config dir>/.claude: a session started in the config repo reads it as PROJECT scope over user scope.
type Nested struct {
	Dir   string   // empty when there is no nested dir
	Files []string // entries in Dir, relative
	Keep  []string // why it cannot be removed safely: uncovered settings keys or non-settings entries
}

// KeepWorktrees is the Keep reason for Claude Code's own worktree dir: legitimate, never a settings leak.
const KeepWorktrees = "worktrees (linked git worktrees — never swept)"

// OnlyWorktrees reports a nested dir that holds nothing but linked worktrees — doctor passes it.
func (n Nested) OnlyWorktrees() bool {
	return len(n.Keep) == 1 && n.Keep[0] == KeepWorktrees && len(n.Files) == 1
}

// NestedConfig inspects the nested dir; it is removable only when every settings key it holds is already set,
// to the same value, in user settings.json, and it holds nothing but settings*.json.
func NestedConfig() Nested {
	dir := filepath.Join(paths.ConfigDir(), ".claude")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Nested{}
	}
	n := Nested{Dir: dir}
	user := readJSON(filepath.Join(paths.ConfigDir(), "settings.json"))
	for _, e := range entries {
		n.Files = append(n.Files, e.Name())
		if e.IsDir() && e.Name() == "worktrees" {
			n.Keep = append(n.Keep, KeepWorktrees)
			continue
		}
		if e.IsDir() || !strings.HasPrefix(e.Name(), "settings") || filepath.Ext(e.Name()) != ".json" {
			n.Keep = append(n.Keep, e.Name()+" (not a settings file)")
			continue
		}
		local := readJSON(filepath.Join(dir, e.Name()))
		if local == nil {
			n.Keep = append(n.Keep, e.Name()+" (unreadable JSON)")
			continue
		}
		for k, v := range local {
			if uv, ok := user[k]; !ok || !reflect.DeepEqual(uv, v) {
				n.Keep = append(n.Keep, e.Name()+":"+k)
			}
		}
	}
	sort.Strings(n.Keep)
	return n
}

// readJSON returns a file's top-level object, or nil when it is missing or not an object.
func readJSON(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}
