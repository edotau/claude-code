package proc

import (
	"net/url"
	"strings"
)

// DeviceHosts lists extra device-identity hosts (Databricks profiles); the caller sets it, as proc stays low-level.
var DeviceHosts func() []string

// deviceSuffixes are domains whose pages need this box's browser: SSO ties to its device identity.
var deviceSuffixes = []string{
	"cloud.databricks.com", "azuredatabricks.net", "gcp.databricks.com",
	"login.microsoftonline.com", "login.windows.net", "login.microsoft.com",
}

// deviceHostURL reports a URL whose host is a device-identity site; match is on a dot boundary, never a substring.
func deviceHostURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if deviceLogin(h, strings.ToLower(u.Path)) || strings.HasPrefix(h, "oauth-openshift.") {
		return true
	}
	for _, s := range deviceSuffixes {
		if h == s || strings.HasSuffix(h, "."+s) {
			return true
		}
	}
	return profileHost(h)
}

func profileHost(h string) bool {
	if DeviceHosts == nil {
		return false
	}
	for _, x := range DeviceHosts() {
		if strings.ToLower(x) == h {
			return true
		}
	}
	return false
}

func deviceLogin(host, path string) bool {
	return (host == "aka.ms" || host == "microsoft.com") && strings.HasPrefix(path, "/devicelogin")
}
