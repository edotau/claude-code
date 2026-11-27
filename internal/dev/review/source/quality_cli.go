package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/edotau/claude-code/internal/platform/textutil"
)

// resolvePath is the absolute path with symlinks collapsed where possible.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// FormatPyFloat mimics Python's str(float): "3.2" not "3.200000", "0.0" not "0".
func FormatPyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// skipParts are directory names never walked; venv, build and dist are decided by isOutputDir.
var skipParts = map[string]bool{"node_modules": true, ".git": true, "__pycache__": true, ".venv": true}

// pathHasSkippedPart mirrors `SKIP_PARTS & set(filepath.parts)`.
func pathHasSkippedPart(path string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if skipParts[part] {
			return true
		}
	}
	return false
}

// isOutputDir reports a venv or build/dist directory holding output; a build/dist with .go files is a package.
func isOutputDir(dir string) bool {
	switch filepath.Base(dir) {
	case "venv":
		_, err := os.Stat(filepath.Join(dir, "pyvenv.cfg"))
		return err == nil
	case "build", "dist":
		entries, err := os.ReadDir(dir)
		return err == nil && !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasSuffix(e.Name(), ".go") })
	}
	return false
}

// walkDir is the traversal seam; tests wrap it to count the paths a walk visits.
var walkDir = filepath.WalkDir

// skipEntry reports a walk entry under a skipped part, or a directory below root that holds output.
func skipEntry(root, p string, d os.DirEntry) bool {
	rel, err := filepath.Rel(root, p)
	return err != nil || pathHasSkippedPart(rel) || (d != nil && d.IsDir() && p != root && isOutputDir(p))
}

// walkSourceFiles calls fn for each non-skipped file under root and returns the paths the walk could not read.
func walkSourceFiles(root string, warn io.Writer, fn func(p string)) []string {
	var unreadable []string
	_ = walkDir(root, func(p string, d os.DirEntry, err error) error {
		if skipEntry(root, p, d) {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err != nil {
			fmt.Fprintf(warn, "warning: skipping unreadable path %s: %v\n", p, err)
			unreadable = append(unreadable, p)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() && !regularLinkTarget(p, d) {
			return nil // a FIFO, device or socket (or a link to one) would block os.ReadFile forever
		}
		fn(p)
		return nil
	})
	return unreadable
}

// regularLinkTarget reports a symlink to a regular file, or a dangling one the reader reports as unreadable.
func regularLinkTarget(p string, d os.DirEntry) bool {
	if d.Type()&os.ModeSymlink == 0 {
		return false
	}
	fi, err := os.Stat(p)
	return err != nil || fi.Mode().IsRegular()
}

// PyMarshalIndent matches Python's json.dumps(v, indent=2): ensure_ascii escaping and no HTML-escaping of <, >, &.
func PyMarshalIndent(v any) ([]byte, error) {
	compact, err := pyMarshalCompact(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// WritePyJSON writes v as PyMarshalIndent plus a newline; the caller words the error.
func WritePyJSON(w io.Writer, v any) error {
	b, err := PyMarshalIndent(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// pyMarshalCompact is PyMarshalIndent's single-line form: no HTML-escaping, ensure_ascii escaping.
func pyMarshalCompact(v any) ([]byte, error) {
	b, err := textutil.MarshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	return pyEscapeNonASCII(b), nil
}

// pyEscapeNonASCII matches Python's json.dumps default ensure_ascii=True (surrogate pairs above U+FFFF).
func pyEscapeNonASCII(b []byte) []byte {
	var out bytes.Buffer
	for _, r := range string(b) {
		if r < 0x80 {
			out.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&out, "\\u%04x\\u%04x", r1, r2)
		} else {
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.Bytes()
}
