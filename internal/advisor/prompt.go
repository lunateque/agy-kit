package advisor

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
)

// BuildSystemPrompt builds the persistent advisor system prompt ported from
// oh-my-pi's Advisor subsystem, appending any discovered WATCHDOG.md guidance.
func BuildSystemPrompt(watchdogs []WatchdogFile) string {
	var b strings.Builder
	b.WriteString(`<system-conventions>
RFC 2119: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL. NEVER=MUST NOT; AVOID=SHOULD NOT.
</system-conventions>

User, code-quality, robustness advocate; peer-shadow main agent.
- Sharpen strategy, problem-solving, judgment; identify cleaner approach.
- Challenge premature "done", thin verification, skipped reasoning.
- Enforce user ask; flag drift immediately.
- Prevent rabbit holes, overthinking, baked-in edge cases.

Cover skipped angles; NEVER re-run reasoning agent already has. Advise before wrong-direction work.

<workflow>
You receive incremental primary-agent transcript deltas (including thinking, tool calls, tool results).
Verify suspicions with your read-only tools (` + "`read`, `grep`, `glob`" + `). NEVER modify files or attempt write actions.
Keep tool investigation tight (0-3 calls normally; deeper only before raising a ` + "`blocker`" + `).
</workflow>

<critical>
- Silence is the DEFAULT when the main agent is on track.
- Advise ONLY on concrete technical risk, broken invariant, or transcript-evident execution failure. Generic uncertainty or vague unease -> SILENT.
- NEVER restate information the agent already saw (compiler errors, test failures, lint output already printed in the transcript).
- NEVER repeat advice you already sent in this session unless the agent ignored it or escalated the same bug.
- When an update ends with ` + "`[in progress — more steps follow]`" + `, the agent is mid-turn: withhold ` + "`nit`" + ` and ` + "`concern`" + `; ONLY raise ` + "`blocker`" + ` for an unrecoverable/destructive side effect or fatal wrong turn executing right now.
- NEVER advise on user intent, asking for clarification, summarizing input, or ceremony.
- NEVER police scope/ambition (large diffs or rewrites are fine unless explicit instruction is breached).
- NEVER raise unsolicited backwards-compatibility concerns unless explicitly required.
- Cite ONLY transcript evidence or files you personally inspected.
</critical>

<severities>
- nit: non-urgent cleanup, simplification, minor edge case.
- concern: material risk, wrong code path, missing constraint, race/security edge case, guessing accessible source/docs instead of checking, thin verification.
- blocker: STOP and reconsider. ONLY when continuing or finishing clearly ships broken/unverified code, contradicts an explicit user instruction (cite it), substitutes stubs/TODOs/mocks for required work, or loops without progress.
</severities>

<output-protocol>
Your final text reply MUST be EITHER:
1. Exactly the single word:
SILENT

2. OR a single raw JSON object (no markdown fences, no extra prose) with up to 1 highest-priority note:
{"severity":"nit|concern|blocker","key":"file_or_area:short_slug","note":"Concise, direct actionable guidance for the main agent (2-6 lines max, cite exact file/line/symbol)."}

Use a stable short ` + "`key`" + ` (e.g. "runner.go:stdin-eof" or "auth:token-leak") so repeated observations deduplicate cleanly.
</output-protocol>
`)

	for _, w := range watchdogs {
		b.WriteString("\nEspecially pay attention to:\n<attention source=\"")
		b.WriteString(html.EscapeString(w.Path))
		b.WriteString("\">\n")
		b.WriteString(html.EscapeString(w.Content))
		b.WriteString("\n</attention>\n")
	}
	return b.String()
}

// ParsedAdvice is the strictly parsed output of an advisor review turn.
type ParsedAdvice struct {
	Silent   bool
	Severity string
	Key      string
	Note     string
}

type rawAdviceJSON struct {
	Severity string `json:"severity"`
	Key      string `json:"key"`
	Note     string `json:"note"`
}

// ParseAdvisorResponse strictly parses the advisor's text output.
// Anything that is "SILENT" or not valid JSON with a valid severity+note is Silent.
func ParseAdvisorResponse(text string) ParsedAdvice {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.EqualFold(trimmed, "SILENT") {
		return ParsedAdvice{Silent: true}
	}

	if strings.HasPrefix(trimmed, "```") {
		if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
			trimmed = trimmed[nl+1:]
		}
		if end := strings.LastIndex(trimmed, "```"); end >= 0 {
			trimmed = trimmed[:end]
		}
		trimmed = strings.TrimSpace(trimmed)
	}

	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return ParsedAdvice{Silent: true}
	}
	candidate := trimmed[start : end+1]

	var raw rawAdviceJSON
	if err := json.Unmarshal([]byte(candidate), &raw); err != nil {
		return ParsedAdvice{Silent: true}
	}

	sev := strings.ToLower(strings.TrimSpace(raw.Severity))
	switch sev {
	case "nit", "concern", "blocker":
	default:
		return ParsedAdvice{Silent: true}
	}

	note := strings.TrimSpace(RedactSecrets(raw.Note))
	if note == "" {
		return ParsedAdvice{Silent: true}
	}

	key := normalizeKey(raw.Key, note)
	return ParsedAdvice{
		Silent:   false,
		Severity: sev,
		Key:      key,
		Note:     note,
	}
}

func normalizeKey(key, note string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		k = strings.ToLower(trimCapUTF8(note, 48))
	}
	var b strings.Builder
	for _, r := range k {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ':' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else if r == ' ' || r == '/' {
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "general"
	}
	return res
}

func SeverityRank(sev string) int {
	switch sev {
	case "blocker":
		return 3
	case "concern":
		return 2
	case "nit":
		return 1
	default:
		return 0
	}
}

// FormatAdvisorySystemMessage renders the model-visible <advisory> XML block.
// Runs RedactSecrets and XML-escapes the body so untrusted text can never break
// out of </advisory> or smuggle raw tags into the primary agent's prompt.
func FormatAdvisorySystemMessage(n ReadyNote) string {
	safeNote := html.EscapeString(RedactSecrets(n.Note))
	return fmt.Sprintf(
		"%s\n<advisory advisor=%q severity=%q key=%q guidance=\"weigh, don't blindly obey\">\n%s\n</advisory>",
		SelfAdvisoryMarker,
		n.AdvisorName,
		n.Severity,
		n.Key,
		safeNote,
	)
}

var mdImageOrLinkRe = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)

// FormatAdvisoryUserMarkdown renders a sanitized inline card shown in agy UI,
// stripping markdown images/links and HTML tags to prevent exfiltration channels.
func FormatAdvisoryUserMarkdown(n ReadyNote) string {
	icon := "💬"
	switch n.Severity {
	case "blocker":
		icon = "🛑"
	case "concern":
		icon = "⚠️"
	case "nit":
		icon = "💡"
	}
	clean := RedactSecrets(n.Note)
	clean = mdImageOrLinkRe.ReplaceAllString(clean, "$1")
	clean = strings.ReplaceAll(clean, "<", "&lt;")
	clean = strings.ReplaceAll(clean, ">", "&gt;")
	clean = strings.ReplaceAll(clean, "\n", " ")
	return fmt.Sprintf("> %s **Advisor (%s · %s):** %s", icon, n.AdvisorName, n.Severity, clean)
}
