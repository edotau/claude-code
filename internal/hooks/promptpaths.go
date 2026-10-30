package hooks

import (
	"fmt"
	"image"
	_ "image/gif"  // registers GIF with image.DecodeConfig
	_ "image/jpeg" // registers JPEG with image.DecodeConfig
	_ "image/png"  // registers PNG with image.DecodeConfig
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

// posixPathRE matches an absolute POSIX path token. Deliberately greedy-but-bounded: every hit is
// then existence-filtered, so over-matching costs a stat, not a false report.
var posixPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`/[^\s"'` + "`" + `,;:()\[\]{}]+`)
})

// quotedPathRE matches a double-quoted absolute POSIX path, allowing spaces inside the quotes.
var quotedPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`"(/[^"]+)"`)
})

// imageExts are the extensions we report as images. Those image.DecodeConfig cannot read (no stdlib
// decoder) still get a type+size line — the point is telling the user the path was recognized.
var imageExts = map[string]string{
	".png": "PNG", ".jpg": "JPEG", ".jpeg": "JPEG", ".gif": "GIF",
	".webp": "WebP", ".bmp": "BMP", ".svg": "SVG", ".tif": "TIFF", ".tiff": "TIFF",
	".heic": "HEIC", ".avif": "AVIF", ".ico": "ICO",
}

// pathReport is one resolved path's user-facing description.
type pathReport struct {
	win     string // original Windows token, "" when the prompt already used a POSIX path
	path    string // resolved POSIX path
	detail  string // "PNG 640×480, 12.3 KB" or "6 B"
	isImage bool
}

// pathToken is a scanned token: raw as written, trimmed with trailing punctuation stripped.
type pathToken struct{ raw, trimmed string }

// scanTokens extracts unique path tokens matched by bare/quoted, quoted forms first so a path with
// spaces isn't split by the bareword regex. trim strips trailing punctuation off bareword matches.
func scanTokens(text string, quoted, bare *regexp.Regexp, trim string) []pathToken {
	seen := map[string]bool{}
	var out []pathToken
	add := func(raw, p string) {
		if p != "" && !seen[raw] {
			seen[raw] = true
			out = append(out, pathToken{raw, p})
		}
	}
	blanked := quoted.ReplaceAllStringFunc(text, func(m string) string {
		q := quoted.FindStringSubmatch(m)[1]
		add(q, q)
		return strings.Repeat(" ", len(m))
	})
	for _, m := range bare.FindAllString(blanked, -1) {
		t := m
		if trim != "" {
			t = strings.TrimRight(m, trim)
		}
		add(m, t)
	}
	return out
}

func scanPosixTokens(text string) []pathToken {
	return scanTokens(text, quotedPathRE(), posixPathRE(), ".,;:!?)")
}

// describe builds the detail string for an existing file: image type+dimensions when decodable,
// else type/size. ok=false for non-regular files (a bare directory match is noise, not an attachment).
func describe(p string) (detail string, isImage bool, ok bool) {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return "", false, false
	}
	kind, isImg := imageExts[strings.ToLower(filepath.Ext(p))]
	if !isImg {
		return humanBytes(fi.Size()), false, true
	}
	if f, err := os.Open(p); err == nil {
		cfg, _, derr := image.DecodeConfig(f)
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "path-resolver: close %s: %v\n", p, cerr)
		}
		if derr == nil {
			return fmt.Sprintf("%s %d×%d, %s", kind, cfg.Width, cfg.Height, humanBytes(fi.Size())), true, true
		}
	}
	return fmt.Sprintf("%s, %s", kind, humanBytes(fi.Size())), true, true
}

// collectReports resolves every path token to a report through the mount-table resolver. Ordered
// foreign forms (Windows, UNC, URL, ~) first, then POSIX, each in prompt order.
func collectReports(prompt string, res *paths.Resolver) []pathReport {
	seen := map[string]bool{}
	var reps []pathReport
	add := func(orig string, toks ...string) {
		var p, detail string
		var isImage, found bool
		for _, tok := range toks { // the token as written first, then punctuation-stripped
			rp, ok := res.Resolve(tok)
			if !ok {
				continue
			}
			if seen[rp] {
				return // the preferred reading is already reported
			}
			if d, img, ok := describe(rp); ok {
				p, detail, isImage, found = rp, d, img, true
				if orig != "" {
					orig = tok
				}
				break
			}
		}
		if !found {
			return
		}
		seen[p] = true
		if orig != "" && orig == p {
			orig = ""
		}
		reps = append(reps, pathReport{win: orig, path: p, detail: detail, isImage: isImage})
	}
	for _, tok := range scanWinTokens(prompt) {
		add(tok.trimmed, tok.raw, tok.trimmed)
		// the POSIX scan must not re-read inside a foreign token
		prompt = strings.ReplaceAll(prompt, tok.raw, strings.Repeat(" ", len(tok.raw)))
	}
	for _, tok := range scanPosixTokens(prompt) {
		add("", tok.raw, tok.trimmed)
	}
	return reps
}

// feedbackMessage is the user-visible body: one line per resolved path, images first so an
// attachment is never buried under incidental file mentions.
func feedbackMessage(reps []pathReport) string {
	ordered := append([]pathReport(nil), reps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].isImage && !ordered[j].isImage })
	var b strings.Builder
	for i, r := range ordered {
		if i > 0 {
			b.WriteString("\n")
		}
		icon := "📄"
		if r.isImage {
			icon = "🖼"
		}
		fmt.Fprintf(&b, "%s %s — %s", icon, filepath.Base(r.path), r.detail)
		if r.win != "" {
			fmt.Fprintf(&b, "  (from %s)", r.win)
		}
		fmt.Fprintf(&b, "\n   %s", r.path)
	}
	return b.String()
}

// modelContext tells the model what resolved and, for an image, that reading the path is how it
// gets the pixels — a dropped path is never auto-attached.
func modelContext(reps []pathReport) string {
	var lines, images []string
	for _, r := range reps {
		if r.win != "" {
			lines = append(lines, fmt.Sprintf("  %s -> %s (%s)", r.win, r.path, r.detail))
		} else {
			lines = append(lines, fmt.Sprintf("  %s (%s)", r.path, r.detail))
		}
		if r.isImage {
			images = append(images, r.path)
		}
	}
	body := "File paths resolved from the prompt (use these for reads and @-references):\n" +
		strings.Join(lines, "\n")
	if len(images) > 0 {
		body += "\nImage path(s) referenced but NOT attached — Read the path to view the image: " +
			strings.Join(images, ", ")
	}
	return body
}

// pathResolverResult is the native form: user-visible feedback plus model-visible context, empty when no path hits.
func pathResolverResult(in *Input, res *paths.Resolver) Result {
	reps := collectReports(in.Prompt, res)
	if len(reps) == 0 {
		return Result{}
	}
	return Result{Context: modelContext(reps), SystemMessage: feedbackMessage(reps)}
}

// nativePathResolver binds pathResolverResult to the host's mount view.
func nativePathResolver(in *Input) Result { return pathResolverResult(in, paths.Default()) }

// foreignHead opens a non-POSIX path token: drive letter, UNC, scheme URL, ~ / ~user, or host:/path.
const foreignHead = `(?:\b[A-Za-z]:[\\/]|\\\\|(?:file|smb|nfs)://|~[A-Za-z0-9_.-]*/|\b(?:[\w.-]+@)?[\w.-]{2,}:/)`

// winPathRE matches a bareword non-POSIX path token; a quoted path (spaces inside) is quotedWinPathRE's.
var winPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(foreignHead + `[^\s"'` + "`" + `]+`)
})

// quotedWinPathRE matches a double-quoted non-POSIX path, allowing spaces inside the quotes.
var quotedWinPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`"(` + foreignHead + `[^"]+)"`)
})

// OnWSL reports whether we're running under WSL — /proc/version carries "microsoft", or
// WSL_DISTRO_NAME is set. Off-WSL the normalizer is a no-op (Windows paths never appear).
func OnWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

const winTrim = ".,;:!?)]}'\""

func scanWinTokens(text string) []pathToken {
	return scanTokens(text, quotedWinPathRE(), winPathRE(), winTrim)
}

// humanBytes renders n as B/KB/MB/GB with one decimal above bytes.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f, suffix := float64(n), []string{"KB", "MB", "GB", "TB"}
	i := -1
	for f >= unit && i < len(suffix)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, suffix[i])
}
