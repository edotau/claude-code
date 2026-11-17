package paths

import "testing"

// Given a path glob, Then another letter case of the same path matches only where the filesystem folds case.
func TestGlobMatchFoldsCase(t *testing.T) {
	for _, fold := range []bool{true, false} {
		old := foldGlobCase
		foldGlobCase = fold
		if got := GlobMatch("/repo/internal/agent/**", "/repo/Internal/Agent/x.go"); got != fold {
			t.Errorf("fold=%v: got %v, want %v", fold, got, fold)
		}
		foldGlobCase = old
	}
}
