package source

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"iter"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

//go:embed retired_words.json
var retiredWordsJSON []byte

var retiredWords = sync.OnceValue(func() vocabulary {
	var v vocabulary
	if err := json.Unmarshal(retiredWordsJSON, &v); err != nil {
		panic(fmt.Errorf("decode embedded retired words: %w", err))
	}
	return v
})

type vocabulary struct {
	Words      map[string]string
	Exceptions map[string][]string
}

type namingAdvisory struct {
	line                     int
	surface, name, word, use string
}

func (a namingAdvisory) message(path string) string {
	return fmt.Sprintf("%s:%d: advisory: retired word %q in %s %q (use %s)", path, a.line, a.word, a.surface, a.name, a.use)
}

func nameSegments(name string) iter.Seq[string] {
	return func(yield func(string) bool) {
		runes := []rune(name)
		start := 0
		for i, r := range runes {
			if r == '_' || r == '-' {
				if start < i && !yield(string(runes[start:i])) {
					return
				}
				start = i + 1
			} else if i > start && camelBoundary(runes, i) {
				if !yield(string(runes[start:i])) {
					return
				}
				start = i
			}
		}
		if start < len(runes) {
			yield(string(runes[start:]))
		}
	}
}

func camelBoundary(r []rune, i int) bool {
	return unicode.IsUpper(r[i]) && (unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1]) ||
		i+1 < len(r) && unicode.IsUpper(r[i-1]) && unicode.IsLower(r[i+1]))
}

func appendNamingAdvice(out []namingAdvisory, line int, surface, name string) []namingAdvisory {
	v := retiredWords()
	start := len(out)
	for s := range nameSegments(name) {
		word := strings.ToLower(s)
		use, ok := v.Words[word]
		if !ok || slices.Contains(v.Exceptions[word], name) {
			continue
		}
		a := namingAdvisory{line: line, surface: surface, name: name, word: word, use: use}
		if !slices.Contains(out[start:], a) {
			out = append(out, a)
		}
	}
	return out
}

func literalString(expr ast.Expr) string {
	lit, ok := ast.Unparen(expr).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, _ := strconv.Unquote(lit.Value)
	return s
}

func jsonTagName(tag *ast.BasicLit) string {
	key, _, _ := strings.Cut(reflect.StructTag(literalString(tag)).Get("json"), ",")
	if key == "-" {
		return ""
	}
	return key
}

func changedLiteralLine(fset *token.FileSet, spans lineSpans, expr ast.Expr) int {
	lit, ok := ast.Unparen(expr).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return 0
	}
	line := fset.PositionFor(lit.Pos(), false).Line
	if !spans.touches(line, line) {
		return 0
	}
	return line
}

func retiredNames(fset *token.FileSet, file *ast.File, spans lineSpans) []namingAdvisory {
	if len(spans) == 0 {
		return nil
	}
	var out []namingAdvisory
	for node := range ast.Preorder(file) {
		if f, ok := node.(*ast.Field); ok && f.Tag != nil {
			if line := changedLiteralLine(fset, spans, f.Tag); line != 0 {
				out = appendNamingAdvice(out, line, "json tag", jsonTagName(f.Tag))
			}
		}
	}
	eachFlagDecl(file, func(fc flagCall) {
		if fc.lit == nil {
			return
		}
		if line := changedLiteralLine(fset, spans, fc.lit); line != 0 {
			out = appendNamingAdvice(out, line, "flag", fc.name)
		}
	})
	slices.SortStableFunc(out, func(a, b namingAdvisory) int { return a.line - b.line })
	return out
}
