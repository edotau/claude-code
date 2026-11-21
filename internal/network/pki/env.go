package pki

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

const (
	envSites, envPipRepo        = "CORP_SITES", "CORP_PIP_REPOSITORY"
	envProxy, envNoProxy        = "CORP_PROXY", "CORP_NO_PROXY"
	blockBegin, blockEnd        = "# >>> claude-code network pki >>>", "# <<< claude-code network pki <<<"
	sitesShape, defaultEnvPerms = "name=nameserver=nexus_url", os.FileMode(0o600)
)

// resolvConf is the nameserver list site detection reads; a var so tests can point it at a fixture.
var resolvConf = "/etc/resolv.conf"

type site struct{ name, nameserver, nexus string }

type kv struct{ key, val string }

func parseSites(raw string) ([]site, error) {
	var sites []site
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	for i, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), "=", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
			return nil, fmt.Errorf("%s entry %d is malformed: want %s", envSites, i+1, sitesShape)
		}
		sites = append(sites, site{strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])})
	}
	return sites, nil
}

func nameservers() map[string]bool {
	data, _ := os.ReadFile(resolvConf)
	set := map[string]bool{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
			set[f[1]] = true
		}
	}
	return set
}

// pickSite returns the forced site by name, else the first whose nameserver is in resolv.conf; nil when none match.
func pickSite(sites []site, forced string) (*site, error) {
	if forced != "" {
		names := make([]string, len(sites))
		for i := range sites {
			if sites[i].name == forced {
				return &sites[i], nil
			}
			names[i] = sites[i].name
		}
		return nil, fmt.Errorf("unknown --site; configured in %s: %s", envSites, strings.Join(names, ", "))
	}
	ns := nameservers()
	for i := range sites {
		if ns[sites[i].nameserver] {
			return &sites[i], nil
		}
	}
	return nil, nil
}

// siteEnv resolves the optional site, Nexus and proxy keys, in the order they are printed and written.
func siteEnv(get func(string) string, forced string) (*site, []kv, error) {
	sites, err := parseSites(get(envSites))
	if err != nil {
		return nil, nil, err
	}
	s, err := pickSite(sites, forced)
	if err != nil {
		return nil, nil, err
	}
	var out []kv
	if s != nil {
		if u, err := url.Parse(s.nexus); err == nil && u.Host != "" {
			out = append(out, kv{"NEXUS_HOST", u.Host})
		}
		if repo := get(envPipRepo); repo != "" {
			out = append(out, kv{"PIP_INDEX_URL", strings.TrimRight(s.nexus, "/") + "/repository/" + repo + "/simple"})
		}
	}
	if p := get(envProxy); p != "" {
		out = append(out, kv{"HTTP_PROXY", p}, kv{"HTTPS_PROXY", p})
	}
	if p := get(envNoProxy); p != "" {
		out = append(out, kv{"NO_PROXY", p})
	}
	return s, out, nil
}

func exportLine(p kv) string {
	v := p.val
	if strings.ContainsAny(v, ", ") {
		v = `"` + v + `"`
	}
	return "export " + p.key + "=" + v
}

func caPairs(bundle string) []kv {
	return []kv{{"SSL_CERT_FILE", bundle}, {"REQUESTS_CA_BUNDLE", bundle}, {"CURL_CA_BUNDLE", bundle}, {"NODE_EXTRA_CA_CERTS", bundle}}
}

// replaceBlock swaps the managed block in content for block, or appends it; text outside the block is untouched.
func replaceBlock(content, block string) (string, error) {
	start := strings.Index(content, blockBegin)
	if start < 0 {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return content + block, nil
	}
	endRel := strings.Index(content[start:], blockEnd)
	if endRel < 0 {
		return "", fmt.Errorf("managed block start without its end marker %q", blockEnd)
	}
	end := start + endRel + len(blockEnd)
	if nl := strings.IndexByte(content[end:], '\n'); nl >= 0 {
		end += nl + 1
	} else {
		end = len(content)
	}
	return content[:start] + block + content[end:], nil
}

// writeEnvBlock rewrites only the managed block of file, keeping its mode (else 0600).
func writeEnvBlock(file string, pairs []kv) error {
	mode := defaultEnvPerms
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		if st, serr := os.Stat(file); serr == nil {
			mode = st.Mode().Perm()
		}
	case !os.IsNotExist(err):
		return err
	}
	lines := []string{blockBegin}
	for _, p := range pairs {
		lines = append(lines, exportLine(p))
	}
	lines = append(lines, blockEnd)
	next, err := replaceBlock(string(data), strings.Join(lines, "\n")+"\n")
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return paths.AtomicWrite(file, []byte(next), mode)
}
