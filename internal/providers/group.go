package providers

// GroupDatabricks is the provider key of every Databricks row.
const GroupDatabricks = "databricks"

// providerLocal files a row the writers could not place under a known provider.
const providerLocal = "local"

// provider is p.Provider ("" for a row built in Go without one).
func (p Provider) provider() string { return p.Provider }
