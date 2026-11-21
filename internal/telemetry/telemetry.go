package telemetry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lunateque/agy-kit/internal/fsutil"
)

func baseDir() string {
	if env := os.Getenv("AGY_TELEMETRY_DIR"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agy-telemetry")
}

// --- Data types ---

type HookInput struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
	ModelName             string   `json:"modelName"`
	InvocationNum         int      `json:"invocationNum"`
	InitialNumSteps       int      `json:"initialNumSteps"`
	ExecutionNum          int      `json:"executionNum"`
	TerminationReason     string   `json:"terminationReason"`
}

type TranscriptStep struct {
	StepIndex       int               `json:"step_index"`
	Source          string            `json:"source"`
	Type            string            `json:"type"`
	Status          string            `json:"status"`
	CreatedAt       string            `json:"created_at"`
	InputTokens     *int64            `json:"input_tokens"`
	CacheReadTokens *int64            `json:"cache_read_tokens"`
	OutputTokens    *int64            `json:"output_tokens"`
	ToolCalls       []json.RawMessage `json:"tool_calls"`
}

type ConvState struct {
	ConversationID  string  `json:"conversation_id"`
	Model           string  `json:"model"`
	StartMs         int64   `json:"start_ms"`
	InitialNumSteps int     `json:"initial_num_steps"`
	LastLoggedStep  int     `json:"last_logged_step"`
	LastTPS         float64 `json:"last_tps"`
	LastDurMs       int64   `json:"last_dur_ms"`
	LastIn          int64   `json:"last_in"`
	LastCache       int64   `json:"last_cache"`
	LastOut         int64   `json:"last_out"`
	LastTiming      string  `json:"last_timing"`
	SessReqs        int     `json:"sess_reqs"`
	SessIn          int64   `json:"sess_in"`
	SessCache       int64   `json:"sess_cache"`
	SessOut         int64   `json:"sess_out"`
	SessTimedOut    int64   `json:"sess_timed_out"`
	SessDurMs       int64   `json:"sess_dur_ms"`
	UpdatedMs       int64   `json:"updated_ms"`
}

func (st ConvState) sessAvgTPS() float64 {
	if st.SessDurMs <= 0 {
		return 0
	}
	out := st.SessTimedOut
	if out <= 0 {
		// Backward compatibility with state files written before SessTimedOut existed.
		out = st.SessOut
	}
	if out <= 0 {
		return 0
	}
	return math.Round((float64(out)/(float64(st.SessDurMs)/1000.0))*10) / 10
}

type Config struct {
	TPS         string `json:"tps"`          // "call" | "avg" | "both"
	Cache       string `json:"cache"`        // "call" | "avg" | "both"
	ShowQuota   bool   `json:"show_quota"`   // show quota buckets
	QuotaFilter string `json:"quota_filter"` // "all" | "active" | "gemini" | "claude" | comma-separated substrings
	ShowCost    bool   `json:"show_cost"`    // show $ cost / session totals
	MaxLines    int    `json:"max_lines"`    // 1 or 2
	ShowAdvisor *bool  `json:"show_advisor,omitempty"` // show advisor badge (default true)
}

func defaultConfig() Config {
	return Config{
		TPS:         "both",
		Cache:       "call",
		ShowQuota:   true,
		QuotaFilter: "all",
		ShowCost:    true,
		MaxLines:    2,
	}
}

func configPath() string {
	return filepath.Join(baseDir(), "config.json")
}

func loadConfig() Config {
	cfg := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	switch cfg.TPS {
	case "call", "avg", "both":
	default:
		cfg.TPS = "both"
	}
	switch cfg.Cache {
	case "call", "avg", "both":
	default:
		cfg.Cache = "call"
	}
	if cfg.MaxLines < 1 {
		cfg.MaxLines = 1
	} else if cfg.MaxLines > 2 {
		cfg.MaxLines = 2
	}
	return cfg
}

func saveConfig(cfg Config) error {
	ensureDirs()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(configPath(), data, 0o644)
}

type RequestRecord struct {
	Timestamp  string  `json:"ts"`
	ConvID     string  `json:"conv_id"`
	Step       int     `json:"step"`
	Model      string  `json:"model"`
	Input      int64   `json:"in"`
	CacheRead  int64   `json:"cache"`
	Output     int64   `json:"out"`
	CachePct   float64 `json:"cache_pct"`
	DurationMs int64   `json:"dur_ms"`
	TPS        float64 `json:"tps"`
	Timing     string  `json:"timing"`
}

// --- Statusline stdin payload ---

type QuotaBucket struct {
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetInSeconds    int64   `json:"reset_in_seconds"`
	Disabled          bool    `json:"disabled"`
}

type StatusLineInput struct {
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	TerminalWidth  int    `json:"terminal_width"`
	Width          int    `json:"width"`
	Columns        int    `json:"columns"`
	Model          struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow struct {
		TotalInputTokens    int64   `json:"total_input_tokens"`
		TotalOutputTokens   int64   `json:"total_output_tokens"`
		ContextWindowSize   int64   `json:"context_window_size"`
		UsedPercentage      float64 `json:"used_percentage"`
		RemainingPercentage float64 `json:"remaining_percentage"`
		CurrentUsage        struct {
			InputTokens          int64 `json:"input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
		} `json:"current_usage"`
	} `json:"context_window"`
	Quota      map[string]QuotaBucket `json:"quota"`
	AgentState string                 `json:"agent_state"`
	Cost       struct {
		TotalUSD  float64 `json:"total_usd"`
		Estimated bool    `json:"estimated"`
	} `json:"cost"`
	PlanTier string `json:"plan_tier"`
}

// --- Helpers ---

func ensureDirs() (string, string) {
	root := baseDir()
	stateDir := filepath.Join(root, "state")
	_ = os.MkdirAll(stateDir, 0o755)
	return root, stateDir
}

func safeID(id string) string {
	if id == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func loadState(path string) ConvState {
	var st ConvState
	st.LastLoggedStep = -1
	data, err := fsutil.ReadFileShared(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

const maxStepDurMs = 300_000

func saveJSONAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".agy-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// On Windows, os.Rename can transiently fail with ERROR_ACCESS_DENIED while
	// the statusline process is reading the target file; retry briefly with backoff.
	var renameErr error
	for attempt := 0; attempt < 8; attempt++ {
		if renameErr = os.Rename(tmp, path); renameErr == nil {
			return nil
		}
		time.Sleep(time.Duration(5*(attempt+1)) * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return renameErr
}

// withConvLock serializes concurrent hook invocations (e.g. post-invocation + stop)
// for the same conversation using an OS kernel file lock (LockFileEx / flock).
// If the lock cannot be acquired within 500ms, fn is skipped rather than running unlocked.
func withConvLock(lockPath string, fn func()) {
	_ = fsutil.WithKernelLock(lockPath, 500*time.Millisecond, fn)
}

func appendRequest(root string, rec RequestRecord) {
	p := filepath.Join(root, "requests.jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
}

func parseTranscript(path string) []TranscriptStep {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var steps []TranscriptStep
	// Use bufio.Reader instead of Scanner so oversized lines (>16MB) never halt parsing.
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 && len(line) <= 16*1024*1024 {
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				var s TranscriptStep
				if json.Unmarshal([]byte(trimmed), &s) == nil {
					steps = append(steps, s)
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return steps
}

func parseRFC3339Ms(ts string) (int64, bool) {
	if ts == "" {
		return 0, false
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UnixMilli(), true
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

func flushUnloggedSteps(root string, st *ConvState, transcriptPath string, model string, nowMs int64, isLiveEnd bool) {
	steps := parseTranscript(transcriptPath)
	if len(steps) == 0 {
		return
	}

	maxStep := -1
	for _, s := range steps {
		if s.StepIndex > maxStep {
			maxStep = s.StepIndex
		}
	}

	for i, s := range steps {
		if s.Type != "PLANNER_RESPONSE" {
			continue
		}
		if s.StepIndex <= st.LastLoggedStep {
			continue
		}
		if s.OutputTokens == nil {
			continue
		}

		inTok := int64(0)
		if s.InputTokens != nil {
			inTok = *s.InputTokens
		}
		cacheTok := int64(0)
		if s.CacheReadTokens != nil {
			cacheTok = *s.CacheReadTokens
		}
		outTok := *s.OutputTokens

		var durMs int64
		timing := "transcript"

		isLatestStep := s.StepIndex == maxStep
		pStart, okStart := parseRFC3339Ms(s.CreatedAt)
		// Only trust PreInvocation StartMs when it aligns with this step's CreatedAt
		// (prevents multi-step tool turns from counting tool runtime as model time).
		startMatchesStep := st.StartMs > 0 && (!okStart || math.Abs(float64(st.StartMs-pStart)) < 5000)

		if isLiveEnd && isLatestStep && startMatchesStep && nowMs > st.StartMs && (nowMs-st.StartMs) <= maxStepDurMs {
			durMs = nowMs - st.StartMs
			timing = "exact"
		} else {
			if startMatchesStep && s.StepIndex >= st.InitialNumSteps {
				pStart = st.StartMs
				okStart = true
			}
			if okStart {
				var nextMs int64
				var okNext bool
				// Only use step_delta when this PLANNER_RESPONSE actually emitted tool calls,
				// and only scan immediate GENERIC tool steps (never SYSTEM_MESSAGE/USER_INPUT).
				if len(s.ToolCalls) > 0 {
					target := -1
					for off := 1; off <= len(s.ToolCalls) && i+off < len(steps); off++ {
						if steps[i+off].Type != "GENERIC" {
							break
						}
						target = i + off
					}
					if target != -1 {
						nextMs, okNext = parseRFC3339Ms(steps[target].CreatedAt)
					}
				}
				if okNext && nextMs > pStart && (nextMs-pStart) <= maxStepDurMs {
					durMs = nextMs - pStart
					timing = "step_delta"
				} else if isLiveEnd && isLatestStep && nowMs > pStart && (nowMs-pStart) <= maxStepDurMs {
					durMs = nowMs - pStart
					timing = "approx"
				}
			}
		}

		if durMs < 50 {
			durMs = 0
		}
		tps := 0.0
		if durMs > 0 && outTok > 0 {
			tps = float64(outTok) / (float64(durMs) / 1000.0)
			tps = math.Round(tps*10) / 10
		}

		cachePct := 0.0
		if denom := inTok + cacheTok; denom > 0 {
			cachePct = math.Round((float64(cacheTok)/float64(denom))*1000) / 10
		}

		m := model
		if m == "" {
			m = st.Model
		}
		if m == "" {
			m = "unknown"
		}

		recTS := time.Now().Format(time.RFC3339)
		if tsMs, ok := parseRFC3339Ms(s.CreatedAt); ok {
			recTS = time.UnixMilli(tsMs).Local().Format(time.RFC3339)
		}

		rec := RequestRecord{
			Timestamp:  recTS,
			ConvID:     st.ConversationID,
			Step:       s.StepIndex,
			Model:      m,
			Input:      inTok,
			CacheRead:  cacheTok,
			Output:     outTok,
			CachePct:   cachePct,
			DurationMs: durMs,
			TPS:        tps,
			Timing:     timing,
		}
		appendRequest(root, rec)

		st.LastLoggedStep = s.StepIndex
		st.Model = m
		st.LastIn = inTok
		st.LastCache = cacheTok
		st.LastOut = outTok
		if durMs > 0 {
			// Migrate pre-SessTimedOut state files once so avg TPS doesn't undercount earlier calls.
			if st.SessDurMs > 0 && st.SessTimedOut == 0 && st.SessOut > 0 {
				st.SessTimedOut = st.SessOut
			}
			st.LastDurMs = durMs
			st.LastTPS = tps
			st.LastTiming = timing
			st.SessTimedOut += outTok
			st.SessDurMs += durMs
		}
		st.SessReqs++
		st.SessIn += inTok
		st.SessCache += cacheTok
		st.SessOut += outTok
		st.UpdatedMs = nowMs
	}
}

// --- Hook handlers ---

func runHook(kind string) {
	root, stateDir := ensureDirs()
	raw, _ := io.ReadAll(os.Stdin)
	var in HookInput
	_ = json.Unmarshal(raw, &in)

	nowMs := time.Now().UnixMilli()
	convID := in.ConversationID
	if convID == "" {
		convID = "default"
	}
	stPath := filepath.Join(stateDir, safeID(convID)+".json")
	lockPath := filepath.Join(stateDir, safeID(convID)+".lock")

	withConvLock(lockPath, func() {
		st := loadState(stPath)
		st.ConversationID = convID

		switch kind {
		case "pre-invocation":
			// On the first hook call of a resumed session, don't backfill older steps.
			if st.LastLoggedStep == -1 && in.InitialNumSteps > 0 {
				st.LastLoggedStep = in.InitialNumSteps - 1
			}
			// Flush any unlogged steps from the previous invocation BEFORE overwriting st.Model.
			if st.StartMs > 0 {
				flushUnloggedSteps(root, &st, in.TranscriptPath, st.Model, nowMs, false)
			}
			if in.ModelName != "" && in.ModelName != "auto" {
				st.Model = in.ModelName
			}
			st.StartMs = nowMs
			st.InitialNumSteps = in.InitialNumSteps
			st.UpdatedMs = nowMs
			_ = saveJSONAtomic(stPath, st)
			_ = saveJSONAtomic(filepath.Join(root, "latest.json"), st)

		case "post-invocation", "stop":
			if in.ModelName != "" && in.ModelName != "auto" {
				st.Model = in.ModelName
			}
			flushUnloggedSteps(root, &st, in.TranscriptPath, st.Model, nowMs, true)
			st.StartMs = 0
			_ = saveJSONAtomic(stPath, st)
			_ = saveJSONAtomic(filepath.Join(root, "latest.json"), st)
		}
	})
	fmt.Print("{}")
}

// --- Statusline renderer ---

const (
	cReset  = "\x1b[0m"
	cDim    = "\x1b[38;5;242m"
	cCyan   = "\x1b[38;5;111m"
	cPurple = "\x1b[38;5;141m"
	cGreen  = "\x1b[38;5;114m"
	cYellow = "\x1b[38;5;221m"
	cRed    = "\x1b[38;5;203m"
	cTeal   = "\x1b[38;5;80m"
	cBold   = "\x1b[1m"
)

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000.0)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000.0)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func fmtDurationShort(sec int64) string {
	if sec <= 0 {
		return "now"
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	if d > 0 {
		return fmt.Sprintf("%dd%dh", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func shortModelName(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"Gemini 4 ", "Gemini ", "Claude ", "gemini-4-", "gemini-", "claude-"} {
		if strings.HasPrefix(s, p) {
			return strings.TrimPrefix(s, p)
		}
	}
	return s
}

func ultraShortModelName(s string) string {
	s = shortModelName(s)
	s = strings.ReplaceAll(s, " (High)", "-H")
	s = strings.ReplaceAll(s, " (XHigh)", "-XH")
	s = strings.ReplaceAll(s, " (Medium)", "-M")
	s = strings.ReplaceAll(s, " (Low)", "-L")
	s = strings.ReplaceAll(s, "-high", "-H")
	s = strings.ReplaceAll(s, "-xhigh", "-XH")
	s = strings.ReplaceAll(s, "-medium", "-M")
	s = strings.ReplaceAll(s, "-low", "-L")
	return s
}

// visLen returns the visible character width of a string with ANSI CSI escapes stripped.
func visLen(s string) int {
	n := 0
	inEsc := false
	for i := 0; i < len(s); {
		if !inEsc && s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i += 2
			continue
		}
		if inEsc {
			b := s[i]
			i++
			if b >= 0x40 && b <= 0x7e {
				inEsc = false
			}
			continue
		}
		_, sz := utf8.DecodeRuneInString(s[i:])
		n++
		i += sz
	}
	return n
}

// truncateANSI hard-truncates s to at most maxVis visible characters while preserving ANSI CSI codes.
func truncateANSI(s string, maxVis int) string {
	if maxVis <= 0 {
		return ""
	}
	if visLen(s) <= maxVis {
		return s
	}
	var b strings.Builder
	vis := 0
	inEsc := false
	hasANSI := false
	for i := 0; i < len(s); {
		if !inEsc && s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			hasANSI = true
			b.WriteByte(s[i])
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		if inEsc {
			ch := s[i]
			b.WriteByte(ch)
			i++
			if ch >= 0x40 && ch <= 0x7e {
				inEsc = false
			}
			continue
		}
		if vis >= maxVis {
			break
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		vis++
		i += sz
	}
	if hasANSI {
		b.WriteString(cReset)
	}
	return b.String()
}

func resolveWidth(in StatusLineInput) int {
	if in.TerminalWidth > 0 {
		return in.TerminalWidth
	}
	if in.Width > 0 {
		return in.Width
	}
	if in.Columns > 0 {
		return in.Columns
	}
	// Prefer live TTY width over COLUMNS env var (which can go stale after pane resizes).
	if w := detectTTYWidth(); w > 0 {
		return w
	}
	if env := os.Getenv("COLUMNS"); env != "" {
		if w, err := strconv.Atoi(strings.TrimSpace(env)); err == nil && w > 0 {
			return w
		}
	}
	return 100
}

type quotaItem struct {
	key      string
	label    string
	pct      float64
	resetSec int64
	priority int // lower = shown first
}

func formatQuotaLabel(k string) string {
	l := strings.ToLower(k)
	replacer := strings.NewReplacer(
		"gemini-argon-wtu-", "arg ",
		"gemini-argon-", "arg ",
		"gemini-neon-", "neo ",
		"gemini-hydrogen-", "hyd ",
		"gemini-flash-", "fls ",
		"gemini-", "gem ",
		"claude-", "cld ",
		"anthropic-", "cld ",
		"openai-", "gpt ",
		"weekly", "7d",
		"5-hour", "5h",
		"5hour", "5h",
		"daily", "1d",
		"_", " ",
		"-", " ",
	)
	l = replacer.Replace(l)
	return strings.Join(strings.Fields(l), " ")
}

func quotaPriority(bucketKey string, activeModel string) int {
	bk := strings.ToLower(bucketKey)
	am := strings.ToLower(activeModel)
	isClaudeModel := strings.Contains(am, "claude") || strings.Contains(am, "opus") || strings.Contains(am, "sonnet")
	isArgonModel := strings.Contains(am, "argon")
	isGPTModel := strings.Contains(am, "gpt")
	is3PBucket := strings.Contains(bk, "claude") || strings.Contains(bk, "anthropic") || strings.Contains(bk, "3p") || strings.Contains(bk, "third")

	switch {
	case (isClaudeModel || isGPTModel) && is3PBucket:
		return 0
	case isArgonModel && strings.Contains(bk, "argon"):
		return 0
	case isGPTModel && (strings.Contains(bk, "gpt") || strings.Contains(bk, "openai")):
		return 0
	case !isClaudeModel && !isGPTModel && strings.Contains(bk, "gemini"):
		return 0
	default:
		return 1
	}
}

func quotaMatchesSingleFilter(bk, token, bucketKey, activeModel string) bool {
	switch token {
	case "all":
		return true
	case "active":
		return quotaPriority(bucketKey, activeModel) == 0
	case "gemini":
		for _, tok := range []string{"gemini", "neon", "argon", "hydrogen", "flash"} {
			if strings.Contains(bk, tok) {
				return true
			}
		}
		return false
	case "claude", "opus", "3p":
		for _, tok := range []string{"claude", "anthropic", "opus", "sonnet", "3p", "third"} {
			if strings.Contains(bk, tok) {
				return true
			}
		}
		return false
	default:
		return strings.Contains(bk, token)
	}
}

func quotaMatchesFilter(bucketKey string, filter string, activeModel string) bool {
	f := strings.ToLower(strings.TrimSpace(filter))
	if f == "" || f == "all" {
		return true
	}
	bk := strings.ToLower(bucketKey)
	for _, part := range strings.Split(f, ",") {
		p := strings.TrimSpace(part)
		if p != "" && quotaMatchesSingleFilter(bk, p, bucketKey, activeModel) {
			return true
		}
	}
	return false
}

func collectQuotas(in StatusLineInput, activeModel string, filter string) []quotaItem {
	if len(in.Quota) == 0 {
		return nil
	}
	items := make([]quotaItem, 0, len(in.Quota))
	for k, q := range in.Quota {
		if q.Disabled {
			continue
		}
		if !quotaMatchesFilter(k, filter, activeModel) {
			continue
		}
		items = append(items, quotaItem{
			key:      k,
			label:    formatQuotaLabel(k),
			pct:      q.RemainingFraction * 100.0,
			resetSec: q.ResetInSeconds,
			priority: quotaPriority(k, activeModel),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority < items[j].priority
		}
		return items[i].key < items[j].key
	})
	return items
}

func renderQuotaToken(q quotaItem, withTimer bool) string {
	qCol := cGreen
	if q.pct <= 15 {
		qCol = cRed
	} else if q.pct <= 40 {
		qCol = cYellow
	}
	if withTimer && q.resetSec > 0 {
		return fmt.Sprintf("%s%s %.0f%%%s %s(%s)%s",
			qCol, q.label, q.pct, cReset, cDim, fmtDurationShort(q.resetSec), cReset)
	}
	return fmt.Sprintf("%s%s %.0f%%%s", qCol, q.label, q.pct, cReset)
}

func fitQuotas(items []quotaItem, maxWidth int, prefix string) string {
	if len(items) == 0 || maxWidth <= 8 {
		return ""
	}
	sep := fmt.Sprintf(" %s·%s ", cDim, cReset)

	// Try full (with reset timers), then without timers
	for _, withTimer := range []bool{true, false} {
		var rendered []string
		for _, it := range items {
			rendered = append(rendered, renderQuotaToken(it, withTimer))
		}
		line := prefix + strings.Join(rendered, sep)
		if visLen(line) <= maxWidth {
			return line
		}
	}

	// Still too wide: greedily pack highest-priority buckets without timers + "+N"
	var kept []string
	for i, it := range items {
		tok := renderQuotaToken(it, false)
		remaining := len(items) - (i + 1)
		suffix := ""
		if remaining > 0 {
			suffix = fmt.Sprintf(" %s+%d%s", cDim, remaining, cReset)
		}
		cand := prefix + strings.Join(append(kept, tok), sep) + suffix
		if visLen(cand) <= maxWidth {
			kept = append(kept, tok)
		} else {
			break
		}
	}
	if len(kept) == 0 {
		return ""
	}
	if rem := len(items) - len(kept); rem > 0 {
		return prefix + strings.Join(kept, sep) + fmt.Sprintf(" %s+%d%s", cDim, rem, cReset)
	}
	return prefix + strings.Join(kept, sep)
}

func tpsColor(tps float64) string {
	if tps < 40 {
		return cYellow
	}
	return cGreen
}

func renderStatusLine(in StatusLineInput, st ConvState, maxWidth int, cfg Config) string {
	if maxWidth < 10 {
		maxWidth = 10
	}
	// Reserve 2 cols margin for terminal/TUI padding
	budget := maxWidth - 2
	sep := fmt.Sprintf(" %s│%s ", cDim, cReset)

	modelName := in.Model.DisplayName
	if modelName == "" {
		modelName = in.Model.ID
	}
	if modelName == "" {
		modelName = st.Model
	}

	ctxSize := in.ContextWindow.ContextWindowSize
	curIn := in.ContextWindow.CurrentUsage.InputTokens
	curCache := in.ContextWindow.CurrentUsage.CacheReadInputTokens
	ctxUsed := curIn + curCache
	usedPct := in.ContextWindow.UsedPercentage
	if usedPct == 0 && ctxSize > 0 && ctxUsed > 0 {
		usedPct = (float64(ctxUsed) / float64(ctxSize)) * 100.0
	}

	// Cache hit rates: per-call vs session average
	hasCallCache := false
	callCachePct := 0.0
	if denom := curIn + curCache; denom > 0 {
		callCachePct = (float64(curCache) / float64(denom)) * 100.0
		hasCallCache = true
	} else if st.LastIn+st.LastCache > 0 {
		callCachePct = (float64(st.LastCache) / float64(st.LastIn+st.LastCache)) * 100.0
		hasCallCache = true
	}

	hasAvgCache := false
	avgCachePct := 0.0
	if denom := st.SessIn + st.SessCache; denom > 0 {
		avgCachePct = (float64(st.SessCache) / float64(denom)) * 100.0
		hasAvgCache = true
	}

	avgTPS := st.sessAvgTPS()

	ctxCol := cGreen
	if usedPct >= 80 {
		ctxCol = cRed
	} else if usedPct >= 50 {
		ctxCol = cYellow
	}

	formatCacheSegment := func(tier int) string {
		switch cfg.Cache {
		case "avg":
			if !hasAvgCache {
				return ""
			}
			if tier == 0 {
				return fmt.Sprintf("%s%.0f%% avg cache%s", cTeal, avgCachePct, cReset)
			}
			return fmt.Sprintf("%s%.0f%%c%s", cTeal, avgCachePct, cReset)
		case "both":
			if hasCallCache && hasAvgCache {
				if tier == 0 {
					return fmt.Sprintf("%s%.0f%%/%.0f%% cache%s", cTeal, callCachePct, avgCachePct, cReset)
				}
				return fmt.Sprintf("%s%.0f%%/%.0f%%c%s", cTeal, callCachePct, avgCachePct, cReset)
			}
			if hasCallCache {
				if tier == 0 {
					return fmt.Sprintf("%s%.0f%% cache%s", cTeal, callCachePct, cReset)
				}
				return fmt.Sprintf("%s%.0f%%c%s", cTeal, callCachePct, cReset)
			}
			return ""
		default: // "call"
			if !hasCallCache {
				return ""
			}
			if tier == 0 {
				return fmt.Sprintf("%s%.0f%% cache%s", cTeal, callCachePct, cReset)
			}
			return fmt.Sprintf("%s%.0f%%c%s", cTeal, callCachePct, cReset)
		}
	}

	formatTPSSegment := func(tier int) string {
		hasCall := st.LastTPS > 0
		hasAvg := avgTPS > 0
		durSec := float64(st.LastDurMs) / 1000.0

		switch cfg.TPS {
		case "avg":
			if !hasAvg {
				return ""
			}
			col := tpsColor(avgTPS)
			switch tier {
			case 0:
				return fmt.Sprintf("%savg:%s %s%.0f t/s%s %s(%dc)%s", cDim, cReset, col, avgTPS, cReset, cDim, st.SessReqs, cReset)
			case 1:
				return fmt.Sprintf("%savg:%s %s%.0f t/s%s", cDim, cReset, col, avgTPS, cReset)
			default:
				return fmt.Sprintf("%s~%.0ft/s%s", col, avgTPS, cReset)
			}
		case "both":
			if hasCall && hasAvg {
				cCol := tpsColor(st.LastTPS)
				aCol := tpsColor(avgTPS)
				switch tier {
				case 0:
					return fmt.Sprintf("%scall:%s %s%.0f t/s%s %s(%s/%.1fs)%s %s· avg%s %s%.0f%s",
						cDim, cReset, cCol, st.LastTPS, cReset, cDim, fmtTokens(st.LastOut), durSec, cReset,
						cDim, cReset, aCol, avgTPS, cReset)
				case 1:
					return fmt.Sprintf("%scall:%s %s%.0f%s%s/%s%s%.0f t/s%s",
						cDim, cReset, cCol, st.LastTPS, cReset, cDim, cReset, aCol, avgTPS, cReset)
				default:
					return fmt.Sprintf("%s%.0f%s%s/%s%s%.0ft/s%s",
						cCol, st.LastTPS, cReset, cDim, cReset, aCol, avgTPS, cReset)
				}
			}
			if hasCall {
				cCol := tpsColor(st.LastTPS)
				switch tier {
				case 0:
					return fmt.Sprintf("%scall:%s %s%.0f t/s%s %s(%s/%.1fs)%s",
						cDim, cReset, cCol, st.LastTPS, cReset, cDim, fmtTokens(st.LastOut), durSec, cReset)
				case 1:
					return fmt.Sprintf("%scall:%s %s%.0f t/s%s", cDim, cReset, cCol, st.LastTPS, cReset)
				default:
					return fmt.Sprintf("%s%.0ft/s%s", cCol, st.LastTPS, cReset)
				}
			}
			if hasAvg {
				aCol := tpsColor(avgTPS)
				return fmt.Sprintf("%savg:%s %s%.0f t/s%s", cDim, cReset, aCol, avgTPS, cReset)
			}
			return ""
		default: // "call"
			if !hasCall {
				return ""
			}
			cCol := tpsColor(st.LastTPS)
			switch tier {
			case 0:
				return fmt.Sprintf("%scall:%s %s%.0f t/s%s %s(%s/%.1fs)%s",
					cDim, cReset, cCol, st.LastTPS, cReset, cDim, fmtTokens(st.LastOut), durSec, cReset)
			case 1:
				return fmt.Sprintf("%scall:%s %s%.0f t/s%s", cDim, cReset, cCol, st.LastTPS, cReset)
			default:
				return fmt.Sprintf("%s%.0ft/s%s", cCol, st.LastTPS, cReset)
			}
		}
	}

	// Build candidate tiers for the main metrics line (tier 0 = richest, tier 2 = ultra-compact)
	buildCore := func(tier int) string {
		var parts []string

		if modelName != "" {
			mn := shortModelName(modelName)
			if tier >= 2 {
				mn = ultraShortModelName(modelName)
			}
			parts = append(parts, fmt.Sprintf("%s%s%s%s", cBold, cCyan, mn, cReset))
		}

		if ctxSize > 0 || usedPct > 0 {
			switch tier {
			case 0:
				if ctxUsed > 0 {
					parts = append(parts, fmt.Sprintf("%sctx %.0f%%%s %s(%s/%s)%s",
						ctxCol, usedPct, cReset, cDim, fmtTokens(ctxUsed), fmtTokens(ctxSize), cReset))
				} else {
					parts = append(parts, fmt.Sprintf("%sctx %.0f%%%s", ctxCol, usedPct, cReset))
				}
			case 1:
				if ctxUsed > 0 {
					parts = append(parts, fmt.Sprintf("%sctx %.0f%%%s %s(%s)%s",
						ctxCol, usedPct, cReset, cDim, fmtTokens(ctxUsed), cReset))
				} else {
					parts = append(parts, fmt.Sprintf("%sctx %.0f%%%s", ctxCol, usedPct, cReset))
				}
			default:
				parts = append(parts, fmt.Sprintf("%s%.0f%%%s", ctxCol, usedPct, cReset))
			}
		}

		tpsSeg := formatTPSSegment(tier)
		cacheSeg := formatCacheSegment(tier)
		if tpsSeg != "" && cacheSeg != "" {
			if tier == 0 {
				parts = append(parts, tpsSeg+fmt.Sprintf(" %s·%s ", cDim, cReset)+cacheSeg)
			} else {
				parts = append(parts, tpsSeg+" "+cacheSeg)
			}
		} else if tpsSeg != "" {
			parts = append(parts, tpsSeg)
		} else if cacheSeg != "" {
			parts = append(parts, fmt.Sprintf("%scall:%s %s", cDim, cReset, cacheSeg))
		} else if in.ContextWindow.TotalOutputTokens > 0 && tier < 2 {
			parts = append(parts, fmt.Sprintf("%sout %s%s", cDim, fmtTokens(in.ContextWindow.TotalOutputTokens), cReset))
		}

		if cfg.ShowCost {
			if in.Cost.TotalUSD > 0 {
				parts = append(parts, fmt.Sprintf("%s$%.2f%s", cPurple, in.Cost.TotalUSD, cReset))
			} else if st.SessReqs > 0 && tier == 0 {
				parts = append(parts, fmt.Sprintf("%ssess: %dc · %s out%s", cDim, st.SessReqs, fmtTokens(st.SessOut), cReset))
			}
		}

		if cfg.ShowAdvisor == nil || *cfg.ShowAdvisor {
			convID := in.ConversationID
			if convID == "" {
				convID = in.SessionID
			}
			if badge := formatAdvisorBadge(convID, tier); badge != "" {
				parts = append(parts, badge)
			}
		}

		return strings.Join(parts, sep)
	}

	var quotas []quotaItem
	if cfg.ShowQuota {
		quotas = collectQuotas(in, modelName, cfg.QuotaFilter)
	}

	// 1. No quotas or strict single-line mode
	if len(quotas) == 0 {
		for tier := 0; tier <= 2; tier++ {
			line := buildCore(tier)
			if visLen(line) <= budget || tier == 2 {
				return truncateANSI(line, budget)
			}
		}
	}

	if cfg.MaxLines == 1 {
		for tier := 0; tier <= 2; tier++ {
			core := buildCore(tier)
			if visLen(core) < budget-12 {
				qBudget := budget - visLen(core) - visLen(sep)
				if qStr := fitQuotas(quotas, qBudget, ""); qStr != "" {
					return truncateANSI(core+sep+qStr, budget)
				}
			}
		}
		return truncateANSI(buildCore(2), budget)
	}

	// 2. Try single-line layout when all quotas fit cleanly without "+N"
	core0 := buildCore(0)
	if visLen(core0) < budget-14 {
		qBudget := budget - visLen(core0) - visLen(sep)
		if qStr := fitQuotas(quotas, qBudget, ""); qStr != "" && !strings.Contains(qStr, "+") {
			return truncateANSI(core0+sep+qStr, budget)
		}
	}

	// 3. Adaptive 2-line layout: Line 1 = model/ctx/tps/cache/cost, Line 2 = quotas
	line1 := core0
	for tier := 0; tier <= 2; tier++ {
		cand := buildCore(tier)
		if visLen(cand) <= budget || tier == 2 {
			line1 = cand
			break
		}
	}
	line1 = truncateANSI(line1, budget)

	qPrefix := fmt.Sprintf("%squota:%s ", cDim, cReset)
	if budget < 60 {
		qPrefix = ""
	}
	line2 := fitQuotas(quotas, budget, qPrefix)
	if line2 == "" {
		return line1
	}
	return line1 + "\n" + truncateANSI(line2, budget)
}

func runStatusLine() {
	root, stateDir := ensureDirs()
	raw, _ := io.ReadAll(os.Stdin)

	var in StatusLineInput
	_ = json.Unmarshal(raw, &in)

	var st ConvState
	convID := in.ConversationID
	if convID == "" {
		convID = in.SessionID
	}
	if convID != "" {
		// Never fall back to another session's latest.json when we know this session's ID.
		st = loadState(filepath.Join(stateDir, safeID(convID)+".json"))
	} else {
		st = loadState(filepath.Join(root, "latest.json"))
	}

	fmt.Print(renderStatusLine(in, st, resolveWidth(in), loadConfig()))
}

type advisorBadgeFile struct {
	V          int    `json:"v"`
	Status     string `json:"status"`
	Advisor    string `json:"advisor"`
	NotesCount int    `json:"notes_count"`
	Pending    int    `json:"pending"`
	UpdatedMs  int64  `json:"updated_ms"`
}

type advisorConfigLite struct {
	Enabled     bool   `json:"enabled"`
	AdvisorName string `json:"advisor_name"`
}

func formatAdvisorBadge(convID string, tier int) string {
	advDir := strings.TrimSpace(os.Getenv("AGY_ADVISOR_DIR"))
	if advDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		advDir = filepath.Join(home, ".agy-advisor")
	}

	// Read global advisor config first so disabled hides the badge immediately and
	// fresh sessions (before first message / before convID is assigned) still show armed state.
	cfgData, cfgErr := fsutil.ReadFileShared(filepath.Join(advDir, "config.json"))
	if cfgErr != nil {
		return ""
	}
	var acfg advisorConfigLite
	if json.Unmarshal(cfgData, &acfg) != nil || !acfg.Enabled {
		return ""
	}
	defaultName := acfg.AdvisorName
	if defaultName == "" {
		defaultName = "Opus"
	}

	var b advisorBadgeFile
	hasConvBadge := false
	if convID != "" {
		badgePath := filepath.Join(advDir, "convs", safeID(convID), "badge.json")
		if data, err := fsutil.ReadFileShared(badgePath); err == nil {
			if json.Unmarshal(data, &b) == nil && b.V == 1 {
				hasConvBadge = true
			}
		}
	}

	if !hasConvBadge {
		// Armed & standing by for first coding turn.
		if tier >= 2 {
			return fmt.Sprintf("%s◎%s", cPurple, cReset)
		}
		return fmt.Sprintf("%s◎ %s%s", cPurple, defaultName, cReset)
	}
	if b.Status == "disabled" {
		return ""
	}
	name := b.Advisor
	if name == "" {
		name = defaultName
	}
	status := b.Status
	// Never display stuck "reviewing" forever if a background worker died mid-call.
	if status == "reviewing" && b.UpdatedMs > 0 && time.Since(time.UnixMilli(b.UpdatedMs)) > 150*time.Second {
		status = "stale"
	}
	switch status {
	case "reviewing":
		if tier >= 2 {
			return fmt.Sprintf("%s◉%s", cCyan, cReset)
		}
		return fmt.Sprintf("%s◉ %s%s", cCyan, name, cReset)
	case "stale":
		return fmt.Sprintf("%s◎ stale%s", cDim, cReset)
	case "quota":
		return fmt.Sprintf("%s◎ quota%s", cYellow, cReset)
	case "backoff":
		return fmt.Sprintf("%s◎ err%s", cRed, cReset)
	default:
		if b.NotesCount > 0 {
			if tier >= 2 {
				return fmt.Sprintf("%s◉%d%s", cGreen, b.NotesCount, cReset)
			}
			return fmt.Sprintf("%s◉ %s:%d%s", cGreen, name, b.NotesCount, cReset)
		}
		if tier >= 2 {
			return fmt.Sprintf("%s◎%s", cPurple, cReset)
		}
		return fmt.Sprintf("%s◎ %s%s", cPurple, name, cReset)
	}
}

// --- Config CLI ---

func runConfig(args []string) int {
	if len(args) == 0 || args[0] == "show" {
		cfg := loadConfig()
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Printf("# %s\n%s\n", configPath(), string(data))
		return 0
	}

	switch args[0] {
	case "init":
		force := len(args) > 1 && (args[1] == "--force" || args[1] == "-f")
		p := configPath()
		if !force {
			if _, err := os.Stat(p); err == nil {
				fmt.Printf("Config already exists: %s (use 'config init --force' to overwrite)\n", p)
				return 0
			}
		}
		if err := saveConfig(defaultConfig()); err != nil {
			fmt.Fprintln(os.Stderr, "failed to write config:", err)
			return 1
		}
		fmt.Println("Wrote default config to", p)

	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: agy-telemetry config set <tps|cache|show_quota|show_cost|max_lines> <value>")
			return 1
		}
		cfg := loadConfig()
		key, val := args[1], strings.ToLower(args[2])
		switch key {
		case "tps":
			if val != "call" && val != "avg" && val != "both" {
				fmt.Fprintln(os.Stderr, "tps must be one of: call, avg, both")
				return 1
			}
			cfg.TPS = val
		case "cache":
			if val != "call" && val != "avg" && val != "both" {
				fmt.Fprintln(os.Stderr, "cache must be one of: call, avg, both")
				return 1
			}
			cfg.Cache = val
		case "show_quota":
			b, err := strconv.ParseBool(val)
			if err != nil {
				fmt.Fprintln(os.Stderr, "show_quota must be true or false")
				return 1
			}
			cfg.ShowQuota = b
		case "quota_filter":
			cfg.QuotaFilter = val
		case "show_cost":
			b, err := strconv.ParseBool(val)
			if err != nil {
				fmt.Fprintln(os.Stderr, "show_cost must be true or false")
				return 1
			}
			cfg.ShowCost = b
		case "max_lines":
			n, err := strconv.Atoi(val)
			if err != nil || (n != 1 && n != 2) {
				fmt.Fprintln(os.Stderr, "max_lines must be 1 or 2")
				return 1
			}
			cfg.MaxLines = n
		default:
			fmt.Fprintln(os.Stderr, "unknown config key:", key)
			return 1
		}
		if err := saveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "failed to save config:", err)
			return 1
		}
		fmt.Printf("Updated %s = %s in %s\n", key, val, configPath())

	default:
		fmt.Fprintln(os.Stderr, "usage: agy-telemetry config [show | init [--force] | set <key> <value>]")
		return 1
	}
	return 0
}

// --- CLI Stats viewer ---

func runStats(args []string) {
	root, _ := ensureDirs()
	p := filepath.Join(root, "requests.jsonl")
	f, err := os.Open(p)
	if err != nil {
		fmt.Println("No telemetry logged yet:", p)
		return
	}
	defer f.Close()

	todayOnly := false
	limit := 15
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "today":
			todayOnly = true
		case "all":
			limit = 100000
		case "last":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					limit = n
					i++
				}
			}
		}
	}

	todayPrefix := time.Now().Format("2006-01-02")
	var recs []RequestRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r RequestRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err == nil {
			if todayOnly && !strings.HasPrefix(r.Timestamp, todayPrefix) {
				continue
			}
			recs = append(recs, r)
		}
	}

	if len(recs) == 0 {
		fmt.Println("No matching requests.")
		return
	}

	type agg struct {
		reqs        int
		inTok       int64
		cache       int64
		outTok      int64
		timedOutTok int64
		durMs       int64
		tpsList     []float64
	}
	byModel := map[string]*agg{}
	for _, r := range recs {
		a := byModel[r.Model]
		if a == nil {
			a = &agg{}
			byModel[r.Model] = a
		}
		a.reqs++
		a.inTok += r.Input
		a.cache += r.CacheRead
		a.outTok += r.Output
		if r.DurationMs > 0 && r.TPS > 0 {
			a.timedOutTok += r.Output
			a.durMs += r.DurationMs
			a.tpsList = append(a.tpsList, r.TPS)
		}
	}

	fmt.Printf("=== Summary by Model (%d calls) ===\n", len(recs))
	fmt.Printf("%-26s %5s %8s %8s %8s %7s %9s %9s\n", "MODEL", "CALLS", "IN", "CACHE", "OUT", "CACHE%", "AVG TPS", "P50 TPS")
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		a := byModel[m]
		cachePct := 0.0
		if a.inTok+a.cache > 0 {
			cachePct = float64(a.cache) / float64(a.inTok+a.cache) * 100.0
		}
		avgTPS := 0.0
		if a.durMs > 0 {
			avgTPS = float64(a.timedOutTok) / (float64(a.durMs) / 1000.0)
		}
		p50 := 0.0
		if len(a.tpsList) > 0 {
			sort.Float64s(a.tpsList)
			p50 = a.tpsList[len(a.tpsList)/2]
		}
		fmt.Printf("%-26s %5d %8s %8s %8s %6.1f%% %9.1f %9.1f\n",
			shortModelName(m), a.reqs, fmtTokens(a.inTok), fmtTokens(a.cache), fmtTokens(a.outTok), cachePct, avgTPS, p50)
	}

	fmt.Printf("\n=== Last %d Calls ===\n", min(limit, len(recs)))
	fmt.Printf("%-19s %-18s %4s %7s %7s %6s %6s %7s %-10s\n", "TIME", "MODEL", "STEP", "IN", "CACHE", "OUT", "DUR", "TPS", "SRC")
	start := len(recs) - limit
	if start < 0 {
		start = 0
	}
	for _, r := range recs[start:] {
		ts := r.Timestamp
		if len(ts) >= 19 {
			ts = strings.ReplaceAll(ts[:19], "T", " ")
		}
		dur := "-"
		if r.DurationMs > 0 {
			dur = fmt.Sprintf("%.1fs", float64(r.DurationMs)/1000.0)
		}
		tps := "-"
		if r.TPS > 0 {
			tps = fmt.Sprintf("%.1f", r.TPS)
		}
		fmt.Printf("%-19s %-18s %4d %7s %7s %6s %6s %7s %-10s\n",
			ts, shortModelName(r.Model), r.Step, fmtTokens(r.Input), fmtTokens(r.CacheRead), fmtTokens(r.Output), dur, tps, r.Timing)
	}
}

func Run(args []string) int {
	if len(args) < 1 {
		fmt.Println("usage: agy-kit telemetry <hook pre-invocation|post-invocation|stop | statusline | stats [today|all|last N] | config [show|init|set <k> <v>]>")
		return 1
	}
	switch args[0] {
	case "hook":
		if len(args) < 2 {
			fmt.Print("{}")
			return 0
		}
		runHook(args[1])
	case "statusline":
		runStatusLine()
	case "stats":
		runStats(args[1:])
	case "config":
		return runConfig(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown telemetry command:", args[0])
		return 1
	}
	return 0
}
