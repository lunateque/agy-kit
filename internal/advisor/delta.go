package advisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

const SelfAdvisoryMarker = "<!-- agy-advisor:v1 -->"
const wipFooter = "---\n\n[in progress — more steps follow]\n"

type transcriptToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type transcriptStep struct {
	StepIndex int                  `json:"step_index"`
	Source    string               `json:"source"`
	Type      string               `json:"type"`
	Status    string               `json:"status"`
	CreatedAt string               `json:"created_at"`
	Content   string               `json:"content"`
	Thinking  string               `json:"thinking"`
	Error     string               `json:"error"`
	ToolCalls []transcriptToolCall `json:"tool_calls"`
}

type renderedSection struct {
	stepIndex int
	text      string
}

// DeltaResult captures newly read steps and cursor advancement.
type DeltaResult struct {
	HasDelta     bool
	ResetEpoch   bool
	HasWork      bool
	FromStep     int
	ToStep       int
	Text         string
	TouchedFiles []string
	NewOffset    int64
	FileSize     int64
	FirstHash    string
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(sk-[a-z0-9_-]{20,})\b`),
	regexp.MustCompile(`(?i)\b(sk-ant-[a-z0-9_-]{20,})\b`),
	regexp.MustCompile(`\b(ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
	regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{30,})\b`),
	regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`),
	regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]{24,}`),
	// Match both complete PEM blocks AND truncated PEM blocks whose END marker was clipped.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+PRIVATE KEY-----.*?(?:-----END [A-Z ]+PRIVATE KEY-----|\z)`),
}

// RedactSecrets masks obvious credential tokens and private keys before shipping text.
func RedactSecrets(s string) string {
	for i, re := range secretPatterns {
		if i == 5 {
			s = re.ReplaceAllString(s, "${1}[REDACTED]")
		} else {
			s = re.ReplaceAllString(s, "[REDACTED_SECRET]")
		}
	}
	return s
}

func hashFirstLine(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	if n == 0 {
		return ""
	}
	line := buf[:n]
	if idx := bytes.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:8])
}

// ReadTranscriptDelta incrementally reads ONLY newly appended newline-terminated
// JSONL lines from transcriptPath starting at cur.ByteOffset.
func ReadTranscriptDelta(transcriptPath string, cur CursorState, maxBytes int, wip bool) DeltaResult {
	res := DeltaResult{
		FromStep:  cur.LastStepIndex + 1,
		ToStep:    cur.LastStepIndex,
		NewOffset: cur.ByteOffset,
		FileSize:  cur.FileSize,
		FirstHash: cur.FirstLineHash,
	}
	if transcriptPath == "" {
		return res
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return res
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return res
	}
	size := info.Size()
	res.FileSize = size
	firstHash := hashFirstLine(f)
	res.FirstHash = firstHash

	offset := cur.ByteOffset
	if size < offset || (cur.FirstLineHash != "" && firstHash != "" && cur.FirstLineHash != firstHash) {
		offset = 0
		res.ResetEpoch = true
		res.FromStep = 0
		res.ToStep = -1
	}

	// Mid-session enable on an already-large transcript: seed to the tail so we
	// don't replay hundreds of historical turns on the first call.
	if cur.ByteOffset == 0 && cur.LastStepIndex < 0 && cur.FirstLineHash == "" && size > 64*1024 {
		tailStart := size - 32*1024
		if _, err := f.Seek(tailStart, io.SeekStart); err == nil {
			buf, _ := io.ReadAll(f)
			if nl := bytes.IndexByte(buf, '\n'); nl >= 0 && nl+1 < len(buf) {
				buf = buf[nl+1:]
				tailStart += int64(nl + 1)
			}
			if lastNL := bytes.LastIndexByte(buf, '\n'); lastNL >= 0 {
				res.NewOffset = tailStart + int64(lastNL+1)
				for _, raw := range bytes.Split(buf[:lastNL], []byte("\n")) {
					var s transcriptStep
					if json.Unmarshal(raw, &s) == nil && s.StepIndex > res.ToStep {
						res.ToStep = s.StepIndex
					}
				}
				return res
			}
		}
	}

	if size <= offset {
		return res
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return res
	}
	raw, err := io.ReadAll(f)
	if err != nil || len(raw) == 0 {
		return res
	}

	// Consume ONLY up to the last '\n' so mid-write partial JSON is never parsed.
	lastNL := bytes.LastIndexByte(raw, '\n')
	if lastNL < 0 {
		return res
	}
	complete := raw[:lastNL]
	res.NewOffset = offset + int64(lastNL+1)

	var steps []transcriptStep
	for _, line := range bytes.Split(complete, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var s transcriptStep
		if err := json.Unmarshal(line, &s); err != nil {
			continue
		}
		if s.Type == "CHECKPOINT" {
			res.ResetEpoch = true
		}
		if !res.ResetEpoch && s.StepIndex <= cur.LastStepIndex {
			continue
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 {
		return res
	}

	var sections []renderedSection
	touchedSet := map[string]bool{}
	lastStep := cur.LastStepIndex
	if res.ResetEpoch {
		lastStep = -1
	}

	for _, s := range steps {
		if s.StepIndex > lastStep {
			lastStep = s.StepIndex
		}

		// Filter self-injected advisories across ALL step types (SYSTEM_MESSAGE or
		// Stop-hook USER_INPUT) so the advisor never reviews its own advice.
		if strings.Contains(s.Content, SelfAdvisoryMarker) {
			continue
		}

		switch s.Type {
		case "USER_INPUT":
			txt := trimCapUTF8(RedactSecrets(cleanUserInput(s.Content)), 4000)
			if txt != "" {
				sections = append(sections, renderedSection{
					stepIndex: s.StepIndex,
					text:      fmt.Sprintf("#### [step %d] User\n%s\n\n", s.StepIndex, txt),
				})
			}
		case "PLANNER_RESPONSE":
			if len(s.ToolCalls) > 0 {
				res.HasWork = true
			}
			var sec strings.Builder
			if th := trimCapUTF8(RedactSecrets(strings.TrimSpace(s.Thinking)), 2500); th != "" {
				fmt.Fprintf(&sec, "<thinking>\n%s\n</thinking>\n", th)
			}
			for _, tc := range s.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Args)
				// Redact BEFORE trimming so PEM private keys longer than 1200 bytes never leak!
				fmt.Fprintf(&sec, "ToolCall `%s`: %s\n", tc.Name, trimCapUTF8(RedactSecrets(string(argsJSON)), 1200))
				extractTouchedFiles(tc, touchedSet)
			}
			if c := trimCapUTF8(RedactSecrets(strings.TrimSpace(s.Content)), 5000); c != "" {
				sec.WriteString(c)
				sec.WriteString("\n")
				if strings.Contains(c, "```") {
					res.HasWork = true
				}
			}
			if sec.Len() > 0 {
				sections = append(sections, renderedSection{
					stepIndex: s.StepIndex,
					text:      fmt.Sprintf("#### [step %d] Assistant\n%s\n", s.StepIndex, sec.String()),
				})
			}
		case "GENERIC":
			res.HasWork = true
			if c := trimCapUTF8(RedactSecrets(strings.TrimSpace(s.Content)), 3000); c != "" {
				sections = append(sections, renderedSection{
					stepIndex: s.StepIndex,
					text:      fmt.Sprintf("#### [step %d] Tool result\n%s\n\n", s.StepIndex, c),
				})
			}
		case "ERROR_MESSAGE":
			res.HasWork = true
			msg := s.Error
			if msg == "" {
				msg = s.Content
			}
			if msg != "" {
				sections = append(sections, renderedSection{
					stepIndex: s.StepIndex,
					text:      fmt.Sprintf("#### [step %d] Error\n%s\n\n", s.StepIndex, trimCapUTF8(RedactSecrets(msg), 1500)),
				})
			}
		case "CHECKPOINT":
			sections = append(sections, renderedSection{
				stepIndex: s.StepIndex,
				text:      fmt.Sprintf("#### [step %d] Context compacted (checkpoint)\n%s\n\n", s.StepIndex, trimCapUTF8(RedactSecrets(s.Content), 4000)),
			})
		case "SYSTEM_MESSAGE":
			if strings.Contains(s.Content, "Reminder: work efficiently") {
				continue
			}
			if c := trimCapUTF8(RedactSecrets(strings.TrimSpace(s.Content)), 600); c != "" {
				sections = append(sections, renderedSection{
					stepIndex: s.StepIndex,
					text:      fmt.Sprintf("#### [step %d] System\n%s\n\n", s.StepIndex, c),
				})
			}
		}
	}

	if len(sections) == 0 {
		res.ToStep = lastStep
		return res
	}

	// Keep the TAIL (newest steps!) on section boundaries when maxBytes is exceeded,
	// preserving both the newest work and the trailing WIP marker.
	header := "### Session update\n\n"
	footer := ""
	if wip {
		footer = wipFooter
	}
	omittedBanner := "... [earlier steps elided to fit budget] ...\n\n"

	budget := maxBytes - len(header) - len(footer)
	if maxBytes <= 0 || budget < 2048 {
		budget = 48 * 1024
	}

	keepFrom := len(sections)
	used := 0
	for i := len(sections) - 1; i >= 0; i-- {
		need := len(sections[i].text)
		if keepFrom == len(sections) {
			// Always keep at least the newest section (trimmed UTF-8 safely if huge).
			keepFrom = i
			used += need
			continue
		}
		if used+need+len(omittedBanner) > budget {
			break
		}
		keepFrom = i
		used += need
	}

	var b strings.Builder
	b.WriteString(header)
	if keepFrom > 0 {
		b.WriteString(omittedBanner)
	}
	for i := keepFrom; i < len(sections); i++ {
		sec := sections[i].text
		if i == keepFrom && len(sections)-keepFrom == 1 && len(sec) > budget {
			sec = trimCapUTF8(sec, budget) + "\n"
		}
		b.WriteString(sec)
	}
	b.WriteString(footer)

	res.HasDelta = true
	res.FromStep = sections[keepFrom].stepIndex
	res.ToStep = lastStep
	res.Text = b.String()
	for fpath := range touchedSet {
		res.TouchedFiles = append(res.TouchedFiles, fpath)
	}
	return res
}

func cleanUserInput(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "<USER_REQUEST>"); idx >= 0 {
		s = s[idx+len("<USER_REQUEST>"):]
		if end := strings.Index(s, "</USER_REQUEST>"); end >= 0 {
			s = s[:end]
		}
	}
	return strings.TrimSpace(s)
}

func extractTouchedFiles(tc transcriptToolCall, set map[string]bool) {
	switch tc.Name {
	case "write_file", "edit_file", "read_file":
		if p, ok := tc.Args["Path"].(string); ok {
			p = strings.Trim(p, `"`)
			if p != "" {
				set[p] = true
			}
		}
	}
}

// trimCapUTF8 caps s to at most maxBytes bytes without ever splitting a multi-byte UTF-8 rune.
func trimCapUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " ...[elided]"
}
