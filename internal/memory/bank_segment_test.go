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
	for _, f := range CorpusFiles(root) {
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
