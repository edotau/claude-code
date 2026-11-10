package agent

// Seams for the external signin tests, which import the vendor packages that register their roster rows.
var (
	StoreSecretSeam = &storeSecret
	RunLoginSeam    = &runLogin
	CredentialSeam  = &credential
)
