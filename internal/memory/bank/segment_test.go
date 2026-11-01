package bank

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

func oracleSearch(root, query string, k int) []SearchHit {
	return oracleRankTermBlocks(ContentTerms(query), k, append(oracleCorpusBlocks(root), oracleRoutedBlocks(root, query)...))
}

// The oracle below is frozen from 5942e1e; only the copied symbols have been renamed.
func oracleSearchAll(query string, k int) []SearchHit {
	blocks := oracleCorpusBlocks(paths.ConfigDir())
	if entries, err := memstore.RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != memstore.HarnessStoreKey() {
				blocks = append(blocks, oracleStoreBlocks(e)...)
			}
		}
	}
	return oracleRankBlocks(query, k, blocks)
}

func oracleRankBlocks(query string, k int, blocks []SearchHit) []SearchHit {
	return oracleRankTermBlocks(ContentTerms(query), k, blocks)
}

func oracleRankTermBlocks(qTerms []string, k int, blocks []SearchHit) []SearchHit {
	if len(qTerms) == 0 {
		return nil
	}
	type doc struct {
		hit SearchHit
		tf  map[string]float64
		len float64
	}
	var docs []doc
	groups := map[string][]int{} // bank\x00file: the segment scope lookupWeighted's compound match searches within
	for _, hit := range blocks {
		tf := map[string]float64{}
		var dl float64
		first, rest, _ := strings.Cut(hit.Block, "\n")
		for _, t := range ContentTerms(first) {
			tf[t] += searchTitleWeight
			dl += searchTitleWeight
		}
		for _, t := range ContentTerms(rest) {
			tf[t]++
			dl++
		}
		if dl == 0 {
			continue
		}
		docs = append(docs, doc{hit, tf, dl})
		key := hit.Bank + "\x00" + hit.File
		groups[key] = append(groups[key], len(docs)-1)
	}
	if len(docs) == 0 {
		return nil
	}
	// weighted[i][t] mirrors SegView.lookupWeighted: per doc, the max of t's own exact tf and
	// compoundWeight times the tf of any other term in the same file containing t as a substring.
	weighted := make([]map[string]float64, len(docs))
	for i := range docs {
		weighted[i] = map[string]float64{}
	}
	for _, idxs := range groups {
		fileTerms := map[string]bool{}
		for _, i := range idxs {
			for t := range docs[i].tf {
				fileTerms[t] = true
			}
		}
		for _, qt := range qTerms {
			for _, i := range idxs {
				tf := docs[i].tf[qt]
				if len(qt) >= compoundMinLen {
					for other := range fileTerms {
						if other != qt && strings.Contains(other, qt) {
							if c := docs[i].tf[other] * compoundWeight; c > tf {
								tf = c
							}
						}
					}
				}
				if tf > 0 {
					weighted[i][qt] = tf
				}
			}
		}
	}
	uniq := map[string]bool{}
	df := map[string]int{}
	for _, qt := range qTerms {
		if uniq[qt] {
			continue
		}
		uniq[qt] = true
		for i := range docs {
			if weighted[i][qt] > 0 {
				df[qt]++
			}
		}
	}
	var avgdl float64
	for _, d := range docs {
		avgdl += d.len
	}
	n := float64(len(docs))
	avgdl /= n
	var hits []SearchHit
	for i, d := range docs {
		var score float64
		matched := map[string]bool{}
		for _, t := range qTerms {
			tf := weighted[i][t]
			if tf == 0 {
				continue
			}
			matched[t] = true
			idf := math.Log((n-float64(df[t])+0.5)/(float64(df[t])+0.5) + 1)
			score += idf * tf * (searchK1 + 1) / (tf + searchK1*(1-searchB+searchB*d.len/avgdl))
		}
		if score > 0 {
			first, _, _ := strings.Cut(d.hit.Block, "\n")
			d.hit.ID = BlockID(first)
			if d.hit.ID == "" {
				d.hit.ID = ContentRef(d.hit.Block)
			}
			d.hit.Score = score
			d.hit.Matched = len(matched)
			hits = append(hits, d.hit)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

type oracleSearchSource struct {
	bank      string
	knowledge bool // whole-file blocks: a knowledge note is one topic, not a heading grammar
	files     []string
	data      map[string][]byte
	native    bool // one-fact topic files with frontmatter, not entry-block bank files
}

func oracleCorpusBlocks(root string) []SearchHit {
	var out []SearchHit
	for _, src := range oracleSearchSources(root) {
		for _, path := range src.files {
			data, loaded := src.data[path]
			if !loaded {
				var err error
				data, err = os.ReadFile(path)
				if err != nil {
					continue
				}
			}
			base := filepath.Base(path)
			if src.knowledge {
				out = append(out, SearchHit{Bank: src.bank, File: base, Block: strings.TrimSpace(stripFrontmatter(string(data))), Dir: ownerDir(src.bank, path)})
				continue
			}
			if src.native {
				if name, desc, body, ok := TopicFrontmatter(string(data)); ok {
					out = append(out, SearchHit{Bank: src.bank, File: base, Block: name + " — " + desc + "\n" + body, Dir: ownerDir(src.bank, path)})
				}
				continue
			}
			_, blocks := EntryBlocks(base, string(data))
			for _, b := range blocks {
				out = append(out, SearchHit{Bank: src.bank, File: base, Block: b, Dir: ownerDir(src.bank, path)})
			}
		}
	}
	return out
}

func oracleSearchSources(root string) []oracleSearchSource {
	bankPaths := func(dir string) []string {
		var fs []string
		for _, name := range bankFiles {
			fs = append(fs, filepath.Join(dir, name))
		}
		return fs
	}
	var out []oracleSearchSource
	if !memstore.IsHomeBank(root) {
		out = append(out, oracleSearchSource{bank: "local", files: bankPaths(memstore.Dir(root))})
	}
	out = append(out, oracleSearchSource{bank: "home", files: bankPaths(memstore.HomeBankDir())})
	if archived, _ := filepath.Glob(filepath.Join(memstore.Dir(root), "archive", "*.md")); len(archived) > 0 {
		out = append(out, oracleSearchSource{bank: "archive", files: archived})
	}
	for _, dir := range NativeDirs(root) {
		if topics, data := oracleTopicContents(dir); len(topics) > 0 {
			out = append(out, oracleSearchSource{bank: "native", files: topics, data: data, native: true})
		}
	}
	// tracked long-term knowledge: cross-repo top plus this repo's subdir
	if files := knowledgeFiles(root); len(files) > 0 {
		out = append(out, oracleSearchSource{bank: "knowledge", files: files, knowledge: true})
	}
	return out
}

func oracleRoutedBlocks(root, query string) []SearchHit {
	own := memstore.Dir(root)
	var out []SearchHit
	for _, e := range memstore.RoutedRepos(query) {
		if e.Dir != own {
			out = append(out, oracleStoreBlocks(e)...)
		}
	}
	return out
}

func oracleStoreBlocks(e memstore.RepoEntry) []SearchHit {
	var out []SearchHit
	for _, name := range bankFiles {
		data, err := os.ReadFile(filepath.Join(e.Dir, name))
		if err != nil {
			continue
		}
		_, blocks := EntryBlocks(name, string(data))
		for _, b := range blocks {
			out = append(out, SearchHit{Bank: e.Key, File: name, Block: b, Dir: e.Dir})
		}
	}
	topics, contents := oracleTopicContents(e.Dir)
	for _, path := range topics {
		if data := contents[path]; data != nil {
			if n, desc, body, ok := TopicFrontmatter(string(data)); ok {
				out = append(out, SearchHit{Bank: e.Key, File: filepath.Base(path), Block: n + " — " + desc + "\n" + body, Dir: e.Dir})
			}
		}
	}
	return out
}

func oracleTopicContents(dir string) ([]string, map[string][]byte) {
	ents, _ := os.ReadDir(dir)
	var files []string
	data := map[string][]byte{}
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if _, ok := ReservedNames[name]; ok {
			continue
		}
		if _, ok := reservedKindFold(name); ok && !hasExactEntry(dir, name) {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil || frontmatterValue(string(raw), "harness_kind") == "bank" {
			continue
		}
		files = append(files, path)
		data[path] = raw
	}
	return files, data
}

// TestCorpusFilesIncludesGlobalNativeTopic: post-cutover, a native topic file living in HomeBankDir() is
// part of a repo's native corpus (bank "native"), not silently dropped by scanning the wrong dir.
func TestCorpusFilesIncludesGlobalNativeTopic(t *testing.T) {
	root := isolatedConfig1(t)
	initStore(t)
	if _, err := memstore.EnsureRepoDir(root); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(memstore.HomeBankDir(), "global-topic.md")
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
