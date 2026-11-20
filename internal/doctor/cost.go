package doctor

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

func costReportCheck(reg *providers.Registry) check {
	c := check{Name: "cost report", Detail: "provider registry unavailable"}
	if reg == nil {
		return c
	}
	names := providers.DatabricksProfiles(reg)
	c.OK = true // an optional feature: nothing set up is not a failure, only a half-set profile is
	if len(names) == 0 {
		c.Detail = "no Databricks profiles"
		return c
	}
	var rows []string
	started := false
	for _, name := range names {
		o := reg.Providers[name].Options
		warehouse := "set"
		var missing []string
		if o.WarehouseID == "" {
			warehouse = "unset"
			missing = append(missing, "options.warehouse_id=<id>")
		}
		if len(o.Prices) == 0 {
			missing = append(missing, `options.prices='{"model-id":{"input":0,"output":0,"cache_read":0,"cache_write":0}}'`)
		}
		started = started || o.WarehouseID != "" || len(o.Prices) > 0
		row := fmt.Sprintf("%s warehouse=%s prices=%d models", name, warehouse, len(o.Prices))
		if len(missing) > 0 {
			c.OK = false
			row += "; set: claude-code configure provider " + name + " " + strings.Join(missing, " ")
		}
		rows = append(rows, row)
	}
	if !started {
		c.OK = true
		c.Detail = "not set up; set per profile: claude-code configure provider <profile> options.warehouse_id=<id> options.prices='{…}'"
		return c
	}
	c.Detail = strings.Join(rows, "; ")
	return c
}
