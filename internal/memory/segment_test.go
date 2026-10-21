package memory

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestRankFilesMatchesOracle(t *testing.T) {
	root := migrateConfig1(t)
	cutover(t)
	local, err := EnsureRepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	routedRoot := gitRepo(t, t.TempDir(), "saturn")
	routed, err := EnsureRepoDir(routedRoot)
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(local, "decisionLog.md"), "# Decisions\n\n"+
		"## router retry <!-- id:1234abcd -->\nretry router latency\n"+
		"```\n## fenced marker\n- fenced marker\n```\n\n"+
		"## retry policy\nrouter retry retry retry\n\n"+
		"## the and of\nwith the and\n")
	write(t, filepath.Join(local, "conventions.md"), "# Conventions\n\n"+
		"- commit small\nreview focused changes\n\n"+
		"- router retry\nidentical tie block\n")
	write(t, filepath.Join(GlobalDir(), "conventions.md"), "# Global\n\n"+
		"- router retry\nidentical tie block\n\n"+
		"- commit small\nreview focused changes\n")
	write(t, filepath.Join(local, "archive", "sessionHistory-old.md"),
		"# Archive\n\n## archive storm\nrouter retry during outage\n")
	write(t, filepath.Join(local, "recall.md"),
		"---\nname: native recall\ndescription: router context\n---\nNative recall preserves retry details.\n")
	write(t, filepath.Join(local, "bank-topic.md"),
		"---\nname: excludedbanktoken\ndescription: excludedbanktoken\nharness_kind: bank\n---\nexcludedbanktoken\n")
	write(t, filepath.Join(KnowledgeDir(), "invariant.md"),
		"---\nname: strippedmetadata\n---\n# knowledge invariant\nRouter retry requires bounded latency.\n")
	write(t, filepath.Join(routed, "decisionLog.md"),
		"# Decisions\n\n## saturn deploy\nRetry deployment after rate limits.\n")
	write(t, filepath.Join(routed, "quasar.md"),
		"---\nname: quasar\ndescription: orbital routing\n---\nSaturn quasar deployment notes.\n")

	queries := []struct {
		query string
		bank  string
		file  string
	}{
		{"router", "local", "decisionLog.md"},
		{"router router retry", "local", "decisionLog.md"},
		{"retry router", "global", "conventions.md"},
		{"commit small", "local", "conventions.md"},
		{"fenced marker", "local", "decisionLog.md"},
		{"native recall", "native", "recall.md"},
		{"knowledge invariant", "knowledge", "invariant.md"},
		{"archive storm", "archive", "sessionHistory-old.md"},
		{"saturn deploy", "saturn", "decisionLog.md"},
		{"saturn quasar", "saturn", "quasar.md"},
		{"the and of", "", ""},
		{"excludedbanktoken", "", ""},
	}
	for _, tt := range queries {
		t.Run(tt.query, func(t *testing.T) {
			searches := []struct {
				name string
				got  []SearchHit
				want []SearchHit
			}{
				{"SearchBanks", SearchBanks(root, tt.query, 0), oracleSearch(root, tt.query, 0)},
				{"SearchAll", SearchAll(tt.query, 0), oracleSearchAll(tt.query, 0)},
			}
			for _, search := range searches {
				if !reflect.DeepEqual(search.got, search.want) {
					t.Errorf("%s(%q) differs from oracle:\ngot:  %#v\nwant: %#v",
						search.name, tt.query, search.got, search.want)
				}
				if tt.bank == "" {
					if search.got != nil {
						t.Errorf("%s(%q) = %#v, want nil", search.name, tt.query, search.got)
					}
					continue
				}
				found := false
				for _, hit := range search.got {
					if hit.Bank == tt.bank && hit.File == tt.file {
						found = true
					}
				}
				if !found {
					t.Errorf("%s(%q) missed intended %s/%s hit: %#v",
						search.name, tt.query, tt.bank, tt.file, search.got)
				}
			}
		})
	}
}

func oracleSearch(root, query string, k int) []SearchHit {
	return oracleRankTermBlocks(contentTerms(query), k, append(oracleCorpusBlocks(root), oracleRoutedBlocks(root, query)...))
}

// The oracle below is frozen from 5942e1e; only the copied symbols have been renamed.
func oracleSearchAll(query string, k int) []SearchHit {
	blocks := oracleCorpusBlocks(paths.ConfigDir())
	if entries, err := RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != harnessStoreKey() {
				blocks = append(blocks, oracleStoreBlocks(e)...)
			}
		}
	}
	return oracleRankBlocks(query, k, blocks)
}

func oracleRankBlocks(query string, k int, blocks []SearchHit) []SearchHit {
	return oracleRankTermBlocks(contentTerms(query), k, blocks)
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
	df := map[string]int{}
	for _, hit := range blocks {
		tf := map[string]float64{}
		var dl float64
		first, rest, _ := strings.Cut(hit.Block, "\n")
		for _, t := range contentTerms(first) {
			tf[t] += searchTitleWeight
			dl += searchTitleWeight
		}
		for _, t := range contentTerms(rest) {
			tf[t]++
			dl++
		}
		if dl == 0 {
			continue
		}
		docs = append(docs, doc{hit, tf, dl})
		for t := range tf {
			df[t]++
		}
	}
	if len(docs) == 0 {
		return nil
	}
	var avgdl float64
	for _, d := range docs {
		avgdl += d.len
	}
	n := float64(len(docs))
	avgdl /= n
	var hits []SearchHit
	for _, d := range docs {
		var score float64
		matched := map[string]bool{}
		for _, t := range qTerms {
			tf := d.tf[t]
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
				out = append(out, SearchHit{Bank: src.bank, File: base, Block: strings.TrimSpace(stripFrontmatter(string(data)))})
				continue
			}
			if src.native {
				if name, desc, body, ok := topicFrontmatter(string(data)); ok {
					out = append(out, SearchHit{Bank: src.bank, File: base, Block: name + " — " + desc + "\n" + body})
				}
				continue
			}
			_, blocks := entryBlocks(base, string(data))
			for _, b := range blocks {
				out = append(out, SearchHit{Bank: src.bank, File: base, Block: b})
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
	if !IsGlobal(root) {
		out = append(out, oracleSearchSource{bank: "local", files: bankPaths(Dir(root))})
	}
	out = append(out, oracleSearchSource{bank: "global", files: bankPaths(GlobalDir())})
	if archived, _ := filepath.Glob(filepath.Join(Dir(root), "archive", "*.md")); len(archived) > 0 {
		out = append(out, oracleSearchSource{bank: "archive", files: archived})
	}
	for _, dir := range NativeDirs(root) {
		if topics, data := oracleTopicContents(dir); len(topics) > 0 {
			out = append(out, oracleSearchSource{bank: "native", files: topics, data: data, native: true})
		}
	}
	if CutOver() { // tracked long-term knowledge: cross-repo top plus this repo's subdir
		if files := knowledgeFiles(filepath.Base(Dir(root))); len(files) > 0 {
			out = append(out, oracleSearchSource{bank: "knowledge", files: files, knowledge: true})
		}
	}
	return out
}

func oracleRoutedBlocks(root, query string) []SearchHit {
	own := Dir(root)
	var out []SearchHit
	for _, e := range RoutedRepos(query) {
		if e.Dir != own {
			out = append(out, oracleStoreBlocks(e)...)
		}
	}
	return out
}

func oracleStoreBlocks(e RepoEntry) []SearchHit {
	var out []SearchHit
	for _, name := range bankFiles {
		data, err := os.ReadFile(filepath.Join(e.Dir, name))
		if err != nil {
			continue
		}
		_, blocks := entryBlocks(name, string(data))
		for _, b := range blocks {
			out = append(out, SearchHit{Bank: e.Key, File: name, Block: b})
		}
	}
	topics, contents := oracleTopicContents(e.Dir)
	for _, path := range topics {
		if data := contents[path]; data != nil {
			if n, desc, body, ok := topicFrontmatter(string(data)); ok {
				out = append(out, SearchHit{Bank: e.Key, File: filepath.Base(path), Block: n + " — " + desc + "\n" + body})
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
		if _, ok := reservedNames[name]; ok {
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
