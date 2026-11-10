package memory

import (
	"path/filepath"
	"testing"
)

// TestCorpusFilesIncludesGlobalNativeTopic: post-cutover, a native topic file living in HomeBankDir() is
// part of a repo's native corpus (bank "native"), not silently dropped by scanning the wrong dir.
func TestCorpusFilesIncludesGlobalNativeTopic(t *testing.T) {
	root := isolatedConfig1(t)
	initStore(t)
	if _, err := EnsureRepoDir(root); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(HomeBankDir(), "global-topic.md")
	write(t, topic, "---\nname: global-topic\n---\n\nglobal fact\n")

	var found bool
	for _, f := range corpusFiles(root) {
		if f.Path == topic {
			found = true
			if f.Bank != "native" {
				t.Errorf("global topic bank = %q, want native", f.Bank)
			}
		}
	}
	if !found {
		t.Errorf("corpusFiles(%s) missing global topic %s", root, topic)
	}
}

// routedFiles lists only the named stores other than root's own: never root's, never an unnamed one.
func TestRoutedFilesExcludesOwnStore(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	roots, dirs := map[string]string{}, map[string]string{}
	for _, name := range []string{"proja", "projb", "projc"} {
		roots[name] = gitRepo(t, t.TempDir(), name)
		dir, err := EnsureRepoDir(roots[name])
		if err != nil {
			t.Fatal(err)
		}
		dirs[name] = dir
	}
	files := routedFiles(roots["proja"], "proja projb")
	if len(files) == 0 {
		t.Fatal("routedFiles listed nothing for the named projb")
	}
	for _, f := range files {
		if f.Bank != "projb" || filepath.Dir(f.Path) != dirs["projb"] {
			t.Errorf("routedFiles listed %+v, want only projb's files under %s", f, dirs["projb"])
		}
	}
}
