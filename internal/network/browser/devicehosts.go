package browser

import (
	"net/url"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// registerDeviceHosts makes the vault's Databricks hosts open on this box's browser, loaded only if a URL needs it.
func registerDeviceHosts() {
	proc.DeviceHosts = func() []string {
		reg, err := providers.Load()
		if err != nil {
			return nil
		}
		var hosts []string
		for _, p := range reg.Providers {
			if p == nil || p.Preset != providers.PresetDatabricks {
				continue
			}
			if u, err := url.Parse(providers.AuthHost(p)); err == nil && u.Hostname() != "" {
				hosts = append(hosts, u.Hostname())
			}
		}
		return hosts
	}
}
