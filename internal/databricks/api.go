package databricks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
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
	About: "GET an allow-listed workspace metadata path with a Databricks profile's credential and print the\n" +
		"JSON body. Read-only: GET only, no redirect is followed, the token is never printed. Allowed:\n" +
		"serving-endpoints, sql/warehouses, sql/statements, scim Me, unity-catalog metastore, catalogs,\n" +
		"schemas and tables (see the error for the exact prefixes). Exit 1 on a non-2xx.",
}

const (
	apiLimit    = 8 << 20 // the listing's body cap
	apiAttempts = 3
)

// apiClient is the HTTP client for api; a package var so tests reach their TLS server.
var apiClient = &http.Client{Timeout: 30 * time.Second}

// apiAllowed is read-only metadata only (no exports, DBFS, tokens, users, jobs); extra is the segments below prefix.
var apiAllowed = []struct {
	prefix string
	extra  int
}{
	{"/api/2.0/serving-endpoints", 1}, // list and get only; key redaction unverified
	{"/api/2.0/sql/warehouses", 1},
	{"/api/2.0/sql/statements", 1},
	{"/api/2.0/preview/scim/v2/Me", 0},
	{"/api/2.1/unity-catalog/metastore_summary", 0},
	{"/api/2.1/unity-catalog/metastores", 2}, // metastores/<id>/systemschemas
	{"/api/2.1/unity-catalog/catalogs", 1},
	{"/api/2.1/unity-catalog/schemas", 1},
	{"/api/2.1/unity-catalog/tables", 1},
}

var apiQuery = regexp.MustCompile(`^[A-Za-z0-9_=&.,-]*$`)

// apiTarget is the cleaned, allow-listed path plus query to send, or why raw is refused; what it returns is what is sent.
func apiTarget(raw string) (string, error) {
	rawPath, query, hasQuery := strings.Cut(raw, "?")
	if hasQuery && !apiQuery.MatchString(query) {
		return "", errors.New("query may hold only letters, digits and _=&.,-")
	}
	p, err := url.PathUnescape(rawPath)
	if err != nil || !plainPath(p) {
		return "", errors.New("path must be plain: no scheme, host, escapes, .., // or whitespace")
	}
	p = path.Clean(p)
	if !apiListed(p) {
		return "", fmt.Errorf("path is not allowed; allowed prefixes: %s", strings.Join(apiPrefixes(), " "))
	}
	if hasQuery && query != "" {
		p += "?" + query
	}
	return p, nil
}

// plainPath is true when an unescaped path still holds no escape, backslash, ?, #, .., // or whitespace.
func plainPath(p string) bool {
	return !strings.ContainsAny(p, `%\?#`) && !strings.Contains(p, "..") && !strings.Contains(p, "//") &&
		strings.IndexFunc(p, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func apiListed(p string) bool {
	for _, a := range apiAllowed {
		rest, ok := strings.CutPrefix(p, a.prefix)
		if ok && (rest == "" || (rest[0] == '/' && strings.Count(rest, "/") <= a.extra)) {
			return true
		}
	}
	return false
}

func apiPrefixes() []string {
	out := make([]string, len(apiAllowed))
	for i, a := range apiAllowed {
		out[i] = a.prefix
	}
	return out
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
	target, err := apiTarget(rest[0])
	if err != nil {
		return failCode(2, "api: %v", err)
	}
	if _, err := providers.ResolveIdentity("", as); err != nil || as == providers.IdentityAuto {
		return failCode(2, "api: --as must be user or sp")
	}
	if err := checkProfileFlag(fs, profile); err != nil {
		return failCode(2, "api: %v", err)
	}
	return apiRun(profile, as, target)
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
	if !slices.Contains(providers.DatabricksProfiles(reg), name) {
		return failCode(2, "api: %q is not a Databricks profile; use one of: %s", name, strings.Join(providers.DatabricksProfiles(reg), ", "))
	}
	p := reg.Providers[name]
	host := providers.PresetHost(p.Host)
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
