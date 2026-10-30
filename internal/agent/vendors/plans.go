package vendors

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// CodexPlan is the ChatGPT plan codex is signed in on ("plus", "pro", "team"…), read offline from the id_token
// claim in codex's auth.json (file or keyring); "" when absent or unreadable. The token is decoded, never
// verified or printed.
func CodexPlan() string {
	b, _ := codexAuthJSON()
	if b == nil {
		return ""
	}
	var auth struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return ""
	}
	parts := strings.Split(auth.Tokens.IDToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.Plan
}

// CodexAccessToken is codex's ChatGPT access token and account id from its auth.json (file or keyring);
// "" when absent. keyringNote is "" normally, or the reason a keyring lookup failed for something other
// than "nothing stored", for callers that want to say why no token was found.
func CodexAccessToken() (token, accountID, keyringNote string) {
	b, note := codexAuthJSON()
	if b == nil {
		return "", "", note
	}
	var auth struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return "", "", ""
	}
	return auth.Tokens.Access, auth.Tokens.Account, ""
}

// copilotPlans maps GitHub's copilot_plan values to the plan names GitHub sells.
var copilotPlans = map[string]string{
	"free": "Free", "individual": "Pro", "individual_pro": "Pro+", "business": "Business", "enterprise": "Enterprise",
}

// CopilotPlanName is the sold name of a copilot_plan value ("individual_pro" → "Pro+"); the raw value when unknown.
func CopilotPlanName(raw string) string {
	if name, ok := copilotPlans[raw]; ok {
		return name
	}
	return raw
}
