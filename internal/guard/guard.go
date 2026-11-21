package guard

import "strings"

// IsBlockedGoogleModel returns true if a model or provider string targets
// Google Antigravity or consumer Gemini CLI OAuth endpoints, which must never
// be routed through third-party harnesses (consumer ToS risk).
func IsBlockedGoogleModel(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	if l == "" {
		return false
	}
	for _, pat := range []string{
		"antigravity",
		"google-gemini-cli",
		"gemini-4-neon",
		"gemini-4-argon",
		"gemini-4-hydrogen",
	} {
		if strings.Contains(l, pat) {
			return true
		}
	}
	return false
}
