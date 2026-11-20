package databricks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/httpx"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var apiPage = helptext.Page{
	Usage: "claude-code databricks api /api/<path> [--profile <name>] [--as user|sp]",
	About: "GET a workspace REST path with the profile's stored credential and print the JSON body.\n" +
		"Read-only: GET only, the path must start with /api/, no redirect is followed, the token is\n" +
		"never printed. Exit 1 on a non-2xx, with its status and body on stderr.",
}

const (
	apiLimit    = 8 << 20 // the listing's body cap
	apiAttempts = 3
)

// apiClient is the HTTP client for api; a package var so tests reach their TLS server.
var apiClient = &http.Client{Timeout: 30 * time.Second}

// apiPath is the one reason a path could send the token to another host or another route; "" when the path is safe.
func apiPath(path string) string {
	switch {
	case strings.Contains(path, "://") || strings.HasPrefix(path, "//") || strings.Contains(path, `\`):
		return "path must not carry a scheme or host"
	case !strings.HasPrefix(path, "/api/"):
		return "path must start with /api/"
	case strings.Contains(path, "..") || strings.Contains(strings.ToLower(path), "%2e"):
		return "path must not contain .."
	case strings.IndexFunc(path, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "path must not contain whitespace"
	}
	return ""
}

// apiGet GETs host+path with p's credential, retrying 429/502/503/504 as the model listing does.
func apiGet(ctx context.Context, p *providers.Provider, host, path string) (int, []byte, error) {
	target := host + path
	if u, err := url.Parse(target); err != nil || u.Host != strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://") {
		return 0, nil, errors.New("path would leave the workspace host")
	}
	for attempt := 1; ; attempt++ {
		status, body, hdr, err := httpx.Do(ctx, httpx.Call{Client: apiClient, URL: target, Limit: apiLimit, NoRedirect: true,
			Authorize: func(h http.Header) error { return providers.Authorize(ctx, p, h) }})
		if !slices.Contains([]int{429, 502, 503, 504}, status) || attempt == apiAttempts {
			return status, body, err
		}
		if werr := textutil.WaitRetry(ctx, attempt, 500*time.Millisecond, hdr.Get("Retry-After")); werr != nil {
			return 0, nil, werr
		}
	}
}

// apiVerb is `databricks api <path>`: an authenticated GET, so no one reaches for curl with a token.
func apiVerb(args []string) int {
	fs := helptext.NewFlagSet("databricks api", apiPage)
	var profile, as string
	profileFlagVar(fs, &profile, profileUsage)
	fs.StringVar(&as, "as", "", "identity to call as: user|sp (default: the profile's identity setting)")
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}
	if why := apiPath(rest[0]); why != "" {
		return failCode(2, "api: %s", why)
	}
	if _, err := providers.ResolveIdentity("", as); err != nil || as == providers.IdentityAuto {
		return failCode(2, "api: --as must be user or sp")
	}
	if err := checkProfileFlag(fs, profile); err != nil {
		return failCode(2, "api: %v", err)
	}
	return apiRun(profile, as, rest[0])
}

func failCode(code int, format string, a ...any) int {
	fail(format, a...)
	return code
}

// apiRun resolves the profile and prints the reply: JSON indented, a non-2xx as status and body on stderr.
func apiRun(profile, as, path string) int {
	reg, err := providers.Load()
	if err != nil {
		return fail("api: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := resolveProfile(ctx, reg, profile)
	if name == "" {
		return fail("api: %v", errNoProfile)
	}
	p, err := reg.Get(name)
	if err != nil {
		return fail("api: %v", err)
	}
	host := providers.AuthHost(p)
	if host == "" {
		return fail("api: %s has no https workspace host", name)
	}
	status, body, err := apiGet(providers.WithIdentity(ctx, as), p, host, path)
	if ue, ok := errors.AsType[*url.Error](err); ok { // the url.Error text names the host
		err = ue.Err
	}
	switch {
	case status/100 != 2 && status != 0:
		return fail("api: HTTP %d: %s", status, textutil.CapRunes(strings.TrimSpace(string(body)), 2000, "..."))
	case err != nil:
		return fail("api: %v", err)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		body = append(pretty.Bytes(), '\n')
	}
	_, _ = os.Stdout.Write(body)
	return 0
}
