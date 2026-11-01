package agent

import (
	"testing"
)

const (
	gwProv   = `"gw":{"protocol":"anthropic","base_url":"https://gw.example/anthropic","auth":{"type":"key","env":"TEST_GW_KEY"},"models":{"opus":"claude-opus-4-6","sonnet":"claude-sonnet-4-6"},"env":{"GW_REGION":"eu","GW_TIER":"x"}}`
	ptProv   = `"pt":{"protocol":"anthropic","base_url":"https://pt.example/anthropic","auth":{"type":"cli"},"models":{"opus":"claude-opus-4-6"},"env":{"GW_REGION":"eu"}}`
	plainGw  = `"plain":{"protocol":"anthropic","base_url":"https://plain.example/anthropic","auth":{"type":"key","env":"TEST_GW_KEY"},"models":{"opus":"claude-opus-4-6"}}`
	noModels = `"fallback":[]`
)

const profileDoc = `{` + noModels + `,"providers":{"anthropic":{` + gwProv + `,` + plainGw + `}}}`

func profileFixtureLaunch(t *testing.T) {
	t.Helper()
	hermetic(t, profileDoc)
	t.Setenv("GW_REGION", "")
	t.Setenv("GW_TIER", "")
}
