package guard

import "testing"

func TestIsBlockedGoogleModel(t *testing.T) {
	blocked := []string{
		"google-antigravity/gemini-4-neon-high",
		"google-gemini-cli/gemini-3.8-flash",
		"gemini-4-neon",
		"gemini-4-argon",
		"gemini-4-hydrogen",
	}
	for _, m := range blocked {
		if !IsBlockedGoogleModel(m) {
			t.Fatalf("expected %q to be blocked", m)
		}
	}
	allowed := []string{
		"anthropic/claude-opus-5-5",
		"openai-codex/gpt-5.6-terra",
		"local-aigw/qwen3.8-flash-next",
		"opus",
		"",
	}
	for _, m := range allowed {
		if IsBlockedGoogleModel(m) {
			t.Fatalf("expected %q to be allowed", m)
		}
	}
}
