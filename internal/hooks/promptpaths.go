package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // registers GIF with image.DecodeConfig
	_ "image/jpeg" // registers JPEG with image.DecodeConfig
	_ "image/png"  // registers PNG with image.DecodeConfig
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
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

// scanPaths extracts unique path tokens matched by bare/quoted, quoted forms first so a path with
// spaces isn't split by the bareword regex. trim strips trailing punctuation off bareword matches.
func scanPaths(text string, quoted, bare *regexp.Regexp, trim string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	blanked := quoted.ReplaceAllStringFunc(text, func(m string) string {
		add(quoted.FindStringSubmatch(m)[1])
		return strings.Repeat(" ", len(m))
	})
	for _, m := range bare.FindAllString(blanked, -1) {
		if trim != "" {
			m = strings.TrimRight(m, trim)
		}
		add(m)
	}
	return out
}

// scanPosixPaths extracts unique absolute POSIX path tokens. Mirrors scanWinPaths.
func scanPosixPaths(text string) []string {
	return scanPaths(text, quotedPathRE(), posixPathRE(), ".,;:!?)")
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
		_ = f.Close()
		if derr == nil {
			return fmt.Sprintf("%s %d×%d, %s", kind, cfg.Width, cfg.Height, humanBytes(fi.Size())), true, true
		}
	}
	return fmt.Sprintf("%s, %s", kind, humanBytes(fi.Size())), true, true
}

// collectReports resolves every path token to a report. Windows tokens translate only when onWSL —
// off WSL that would invent a path. Ordered Windows-first, then POSIX, each in prompt order.
func collectReports(prompt string, onWSL bool) []pathReport {
	seen := map[string]bool{}
	var reps []pathReport
	add := func(win, p string) {
		if p == "" || seen[p] {
			return
		}
		detail, isImage, ok := describe(p)
		if !ok {
			return
		}
		seen[p] = true
		reps = append(reps, pathReport{win: win, path: p, detail: detail, isImage: isImage})
	}
	if onWSL {
		for _, win := range scanWinPaths(prompt) {
			add(win, TranslateWinPath(win))
		}
	}
	for _, p := range scanPosixPaths(prompt) {
		add("", p)
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

// feedbackPayload marshals systemMessage (user-visible) and additionalContext (model-visible) in one
// envelope, so a single hook write serves both audiences.
func feedbackPayload(reps []pathReport, vendor ...string) string {
	payload := map[string]any{
		"systemMessage": feedbackMessage(reps),
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": modelContext(reps),
		},
	}
	if len(vendor) > 0 && vendor[0] == VendorCopilot {
		payload = map[string]any{"additionalContext": modelContext(reps)}
	}
	out, _ := json.Marshal(payload)
	return string(out)
}

// promptPathFeedback is the hook core with WSL detection injected, so tests exercise both legs
// without depending on the host. Silent when the prompt names no existing file.
func promptPathFeedback(r io.Reader, stdout io.Writer, onWSL func() bool) int {
	in := ParseInput(r)
	reps := collectReports(in.Prompt, onWSL())
	if len(reps) == 0 {
		return ExitProceed
	}
	fmt.Fprintln(stdout, feedbackPayload(reps, in.Vendor()))
	return ExitProceed
}

// winPathRE matches a bareword Windows path token; a quoted path (spaces inside) is quotedWinPathRE's.
var winPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`[A-Za-z]:[\\/][^\s"']+`)
})

// quotedWinPathRE matches a double-quoted Windows path, allowing spaces inside the quotes.
var quotedWinPathRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`"([A-Za-z]:[\\/][^"]+)"`)
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

// wslpathTimeout bounds one `wslpath` call: hung WSL interop must fall back, never stall UserPromptSubmit.
const wslpathTimeout = time.Second

// TranslateWinPath converts a Windows path to its WSL form. Prefers `wslpath -u` (handles every
// grammar); falls back to a /mnt/<drive> mapping when wslpath is absent.
func TranslateWinPath(win string) string {
	ctx, cancel := context.WithTimeout(context.Background(), wslpathTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "wslpath", "-u", win).Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	drive := strings.ToLower(string(win[0]))
	rest := strings.ReplaceAll(win[2:], `\`, "/")
	return "/mnt/" + drive + "/" + strings.TrimPrefix(rest, "/")
}

// scanWinPaths is scanPosixPaths for Windows tokens.
func scanWinPaths(text string) []string {
	return scanPaths(text, quotedWinPathRE(), winPathRE(), "")
}

// WSLPathNormalize (UserPromptSubmit) reports every existing file path the prompt names, mapping
// dragged Windows paths to /mnt form, for BOTH the model (added context) and user (systemMessage). No-op off WSL.
func WSLPathNormalize(r io.Reader, stdout io.Writer) int {
	return promptPathFeedback(r, stdout, OnWSL)
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
