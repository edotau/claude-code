package docs

import (
	"fmt"
	"image"
	_ "image/gif"  // registers GIF with image.DecodeConfig
	_ "image/jpeg" // registers JPEG with image.DecodeConfig
	_ "image/png"  // registers PNG with image.DecodeConfig
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/clipboard"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// posixPathRE matches an absolute POSIX path token, not the tail of a relative path; hits are existence-filtered.
var posixPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^\p{L}\p{N}_.~/])(/(?:\\ |[^\s"'` + "`" + `,;:()\[\]{}])+)`)
})

// quotedPathRE matches a double- or single-quoted absolute POSIX path, allowing spaces inside the quotes.
var quotedPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`"(/[^"]+)"|'(/[^']+)'`)
})

// fetchClient pulls a client-only path over the SSH bridge; swapped in tests.
var fetchClient = clipboard.FetchClient

// bridgeReady reports an SSH session whose forwarded clipboard socket is present and ours; swapped in tests.
var bridgeReady = clipboard.BridgeReady

// imageExts are the extensions reported as images; undecodable ones still get a type+size line.
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
	large   bool   // at least largeFile bytes
	err     string // fetch failure for a client-only path; path is then the client path
}

// pathToken is a scanned token: raw as written, trimmed with trailing punctuation stripped.
type pathToken struct{ raw, trimmed string }

// tokenScan is one token family: its quoted and bare patterns, each with a cheap test that rules a text out.
type tokenScan struct {
	quoted, bare       func() *regexp.Regexp
	mayQuoted, mayBare func(string) bool // false only when the pattern cannot match the text
	trim               string
	escSpace           bool // a bare match may hold `\ `, so a space after a backslash splits no field
}

// scanTokens extracts unique path tokens, quoted forms first so spaced paths aren't split by the bareword regex.
func scanTokens(text string, s tokenScan) []pathToken {
	seen := map[string]bool{}
	var out []pathToken
	add := func(raw, p string) {
		if p != "" && !seen[raw] {
			seen[raw] = true
			out = append(out, pathToken{raw, p})
		}
	}
	blanked := text
	if s.mayQuoted(text) {
		quoted := s.quoted()
		blanked = quoted.ReplaceAllStringFunc(text, func(m string) string {
			q := strings.Join(quoted.FindStringSubmatch(m)[1:], "")
			add(q, q)
			return strings.Repeat(" ", len(m))
		})
	}
	for _, sub := range fieldMatches(s.bare, blanked, s.escSpace, s.mayBare) {
		m := sub[len(sub)-1] // group 1 when the pattern has a boundary group, else the whole match
		t := m
		if s.trim != "" {
			t = strings.TrimRight(m, s.trim)
		}
		add(m, t)
	}
	return out
}

// fieldMatches equals re.FindAllStringSubmatch(text, -1): no bare match spans a split, and ^ or \b see a field as text.
func fieldMatches(re func() *regexp.Regexp, text string, escSpace bool, may func(string) bool) [][]string {
	var out [][]string
	start := 0
	for i := 0; i <= len(text); i++ {
		if i < len(text) && !splitsField(text, i, escSpace) {
			continue
		}
		if f := text[start:i]; f != "" && may(f) {
			out = append(out, re().FindAllStringSubmatch(f, -1)...)
		}
		start = i + 1
	}
	return out
}

// splitsField reports a regexp \s byte no bare match can hold; with escSpace a space after a backslash may be held.
func splitsField(text string, i int, escSpace bool) bool {
	switch text[i] {
	case '\t', '\n', '\f', '\r':
		return true
	case ' ':
		return !escSpace || i == 0 || text[i-1] != '\\'
	}
	return false
}

// posixScan is the POSIX token family; every match holds a "/" and a quoted one opens with `"/` or `'/`.
var posixScan = tokenScan{
	quoted: quotedPathRE, bare: posixPathRE, trim: ".,;:!?)", escSpace: true,
	mayQuoted: func(t string) bool { return strings.Contains(t, `"/`) || strings.Contains(t, `'/`) },
	mayBare:   func(t string) bool { return strings.Contains(t, "/") },
}

func scanPosixTokens(text string) []pathToken {
	toks := scanTokens(text, posixScan)
	for i := range toks {
		toks[i].trimmed = strings.ReplaceAll(toks[i].trimmed, `\ `, " ")
	}
	return toks
}

// largeFile is the size from which the model is told a file's size before reading it.
const largeFile = 1 << 20

// describe is the detail string for an existing file (image type+size, else type/size); ok=false for non-regular files.
func describe(p string) (detail string, isImage, large, ok bool) {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false, false, false
	}
	large = fi.Size() >= largeFile
	size := strings.ReplaceAll(textutil.HumanBytes(fi.Size()), "iB", "B")
	kind, isImg := imageExts[strings.ToLower(filepath.Ext(p))]
	if !isImg {
		return size, false, large, true
	}
	if f, err := os.Open(p); err == nil {
		cfg, _, derr := image.DecodeConfig(f)
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "path-resolver: close %s: %v\n", p, cerr)
		}
		if derr == nil {
			return fmt.Sprintf("%s %d×%d, %s", kind, cfg.Width, cfg.Height, size), true, large, true
		}
	}
	return fmt.Sprintf("%s, %s", kind, size), true, large, true
}

// collectReports resolves each path token via the mount-table resolver, foreign forms first, in prompt order.
func collectReports(prompt string, res *paths.Resolver) []pathReport {
	win := scanWinTokens(prompt)
	var posix []pathToken
	if len(win) == 0 {
		posix = scanPosixTokens(prompt)
	}
	return collectScanned(prompt, win, posix, res)
}

// collectScanned is collectReports over prompt's scans; posix is reused only when no foreign token blanks the prompt.
func collectScanned(prompt string, win, posix []pathToken, res *paths.Resolver) []pathReport {
	c := &reportCollector{res: res, seen: map[string]bool{}}
	for _, tok := range win {
		c.add(tok.trimmed, tok.raw, tok.trimmed)
		// the POSIX scan must not re-read inside a foreign token
		prompt = strings.ReplaceAll(prompt, tok.raw, strings.Repeat(" ", len(tok.raw)))
	}
	if len(win) > 0 {
		posix = scanPosixTokens(prompt)
	}
	for _, tok := range posix {
		n := len(c.reps)
		c.add("", tok.raw, tok.trimmed)
		if len(c.reps) == n {
			c.fetchRemote(tok.trimmed)
		}
	}
	return c.reps
}

// fetchRemote pulls a /Users or /Volumes path that exists only on the SSH client; reports the local copy or the error.
func (c *reportCollector) fetchRemote(client string) {
	if !strings.HasPrefix(client, "/Users/") && !strings.HasPrefix(client, "/Volumes/") {
		return
	}
	if _, err := os.Lstat(client); err == nil || c.seen[client] || !bridgeReady() {
		return
	}
	c.seen[client] = true
	local, err := fetchClient(client)
	if err != nil {
		c.reps = append(c.reps, pathReport{path: client, err: err.Error()})
		return
	}
	detail, isImage, large, ok := describe(local)
	if !ok {
		c.reps = append(c.reps, pathReport{path: client, err: "fetched copy " + local + " is not a regular file"})
		return
	}
	c.seen[local] = true
	c.reps = append(c.reps, pathReport{win: client, path: local, detail: detail, isImage: isImage, large: large})
}

// reportCollector accumulates path reports, skipping paths it has already reported.
type reportCollector struct {
	res  *paths.Resolver
	seen map[string]bool
	reps []pathReport
}

// add reports the first token (as written, then punctuation-stripped) that resolves to a describable path.
func (c *reportCollector) add(orig string, toks ...string) {
	var p, detail string
	var isImage, large, found bool
	for _, tok := range toks {
		rp, ok := c.res.Resolve(tok)
		if !ok {
			continue
		}
		if c.seen[rp] {
			return // the preferred reading is already reported
		}
		if d, img, lg, ok := describe(rp); ok {
			p, detail, isImage, large, found = rp, d, img, lg, true
			if orig != "" {
				orig = tok
			}
			break
		}
	}
	if !found {
		return
	}
	c.seen[p] = true
	if orig != "" && orig == p {
		orig = ""
	}
	c.reps = append(c.reps, pathReport{win: orig, path: p, detail: detail, isImage: isImage, large: large})
}

// feedbackMessage is one line per resolved path, images first.
func feedbackMessage(reps []pathReport) string {
	ordered := append([]pathReport(nil), reps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].isImage && !ordered[j].isImage })
	var b strings.Builder
	for i, r := range ordered {
		if i > 0 {
			b.WriteString("\n")
		}
		if r.err != "" {
			fmt.Fprintf(&b, "⚠ %s — not fetched from the SSH client: %s", filepath.Base(r.path), r.err)
			continue
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

// modelContext lists only what the prompt text lacks: a mapped or failed path, an image, a large file's size.
func modelContext(reps []pathReport) string {
	var lines, images []string
	for _, r := range reps {
		switch {
		case r.err != "":
			lines = append(lines, assets.HookPathFailed(r.path, r.err))
		case r.win != "":
			lines = append(lines, assets.HookPathMapped(r.win, r.path, r.detail))
		case r.isImage || r.large:
			lines = append(lines, assets.HookPathDetail(r.path, r.detail))
		}
		if r.isImage {
			images = append(images, r.path)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return assets.HookPathsContext(strings.Join(lines, "\n"), strings.Join(images, ", "))
}

// pathResolverResult is the native form: user-visible feedback plus model-visible context, empty when no path hits.
func pathResolverResult(in *core.Input, res *paths.Resolver) core.Result {
	return reportResult(collectReports(in.Prompt, res))
}

// reportResult is the hook result for reps: nothing when no path resolved.
func reportResult(reps []pathReport) core.Result {
	if len(reps) == 0 {
		return core.Result{}
	}
	return core.Result{Context: modelContext(reps), SystemMessage: feedbackMessage(reps)}
}

// defaultResolver reads the host's mount table (macOS runs `mount`); swapped in tests.
var defaultResolver = paths.Default

// ResolvePaths is pathResolverResult on the host's mount view, read only when a token needs the mount table.
func ResolvePaths(in *core.Input) core.Result {
	if core.AutomatedDelivery(in.Prompt) {
		return core.Result{}
	}
	if !strings.ContainsAny(in.Prompt, `/\`) { // every token pattern holds a "/" or a "\"
		return core.Result{}
	}
	win, posix := scanWinTokens(in.Prompt), scanPosixTokens(in.Prompt)
	if len(win)+len(posix) == 0 {
		return core.Result{}
	}
	res := &paths.Resolver{} // a plain absolute path never consults the mount table
	if len(win) > 0 || slices.ContainsFunc(posix, func(t pathToken) bool { return strings.HasPrefix(t.trimmed, "//") }) {
		res = defaultResolver()
	}
	return reportResult(collectScanned(in.Prompt, win, posix, res)) // the scans above are pure: reuse, not rescan
}

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

// winScan is the non-POSIX token family; each of its patterns needs a foreignHead.
var winScan = tokenScan{
	quoted: quotedWinPathRE, bare: winPathRE, trim: ".,;:!?)]}'\"", mayQuoted: mayHoldForeign, mayBare: mayHoldForeign,
}

func scanWinTokens(text string) []pathToken {
	return scanTokens(text, winScan)
}

// mayHoldForeign is false only when no foreignHead alternative can match: each needs ":/", `:\`, `\\` or "~".
func mayHoldForeign(text string) bool {
	return strings.Contains(text, ":/") || strings.Contains(text, `:\`) || strings.Contains(text, `\\`) ||
		strings.Contains(text, "~")
}
