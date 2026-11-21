package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVisLen(t *testing.T) {
	s := cBold + cCyan + "Neon (High)" + cReset + " │ " + cGreen + "94%" + cReset
	if got := visLen(s); got != 17 {
		t.Fatalf("visLen = %d, want 17", got)
	}
}

func sampleInputs() (StatusLineInput, ConvState) {
	var in StatusLineInput
	in.Model.DisplayName = "Gemini 4 Neon (High)"
	in.ContextWindow.ContextWindowSize = 1048576
	in.ContextWindow.UsedPercentage = 12.1
	in.ContextWindow.CurrentUsage.InputTokens = 2500
	in.ContextWindow.CurrentUsage.CacheReadInputTokens = 124500
	in.Cost.TotalUSD = 0.42
	in.Quota = map[string]QuotaBucket{
		"gemini-5-hour":       {RemainingFraction: 0.92, ResetInSeconds: 11520},
		"gemini-weekly":       {RemainingFraction: 0.94, ResetInSeconds: 560580},
		"gemini-argon-weekly": {RemainingFraction: 1.00, ResetInSeconds: 604800},
		"claude-5-hour":       {RemainingFraction: 0.85, ResetInSeconds: 9000},
		"claude-weekly":       {RemainingFraction: 0.90, ResetInSeconds: 400000},
	}

	st := ConvState{
		LastTPS:      118.4,
		LastDurMs:    14500,
		LastOut:      1714,
		LastIn:       2500,
		LastCache:    124500,
		SessReqs:     42,
		SessIn:       277200,
		SessCache:    2700000,
		SessOut:      53600,
		SessTimedOut: 51500,
		SessDurMs:    500000, // 103.0 avg t/s
	}
	return in, st
}

func TestAdaptiveStatusLineFitsWidth(t *testing.T) {
	in, st := sampleInputs()
	cfg := defaultConfig()

	for _, w := range []int{20, 30, 55, 72, 95, 122, 180} {
		out := renderStatusLine(in, st, w, cfg)
		lines := strings.Split(out, "\n")
		for i, line := range lines {
			if l := visLen(line); l > w-2 {
				t.Errorf("width=%d line %d overflowed: visLen=%d > %d\nline=%q", w, i, l, w-2, line)
			}
		}
	}

	// Default config ("both") on 122 cols shows both call and avg TPS, and wraps quotas to line 2
	out122 := renderStatusLine(in, st, 122, cfg)
	if !strings.Contains(out122, "\n") {
		t.Errorf("expected 2-line wrap on 122 cols with 5 quota buckets, got: %q", out122)
	}
	if !strings.Contains(out122, "call:") || !strings.Contains(out122, "avg") {
		t.Errorf("expected both call and avg TPS in default mode, got: %q", out122)
	}
}

func TestConfigModes(t *testing.T) {
	in, st := sampleInputs()

	// avg-only TPS + both cache
	cfg := defaultConfig()
	cfg.TPS = "avg"
	cfg.Cache = "both"
	out := renderStatusLine(in, st, 122, cfg)
	if !strings.Contains(out, "avg:") || !strings.Contains(out, "103 t/s") {
		t.Errorf("expected avg TPS 103 t/s, got: %q", out)
	}
	if !strings.Contains(out, "98%/91% cache") {
		t.Errorf("expected call/avg cache 98%%/91%%, got: %q", out)
	}

	// max_lines = 1 forces single line without overflow
	cfg1 := defaultConfig()
	cfg1.MaxLines = 1
	out1 := renderStatusLine(in, st, 122, cfg1)
	if strings.Contains(out1, "\n") {
		t.Errorf("max_lines=1 must not wrap, got: %q", out1)
	}
	if l := visLen(out1); l > 120 {
		t.Errorf("max_lines=1 overflowed: visLen=%d > 120", l)
	}

	// quota_filter = gemini hides claude buckets
	cfgG := defaultConfig()
	cfgG.QuotaFilter = "gemini"
	outG := renderStatusLine(in, st, 122, cfgG)
	if strings.Contains(outG, "cld") {
		t.Errorf("quota_filter=gemini should hide claude buckets, got: %q", outG)
	}
	// quota_filter = gemini,claude expands aliases (argon, opus, 3p third-party) and excludes gpt
	in.Quota["3p-5-hour"] = QuotaBucket{RemainingFraction: 0.58, ResetInSeconds: 17200}
	in.Quota["gpt-oss-weekly"] = QuotaBucket{RemainingFraction: 0.99}
	cfgGC := defaultConfig()
	cfgGC.QuotaFilter = "gemini,claude"
	outGC := renderStatusLine(in, st, 150, cfgGC)
	if strings.Contains(outGC, "gpt") {
		t.Errorf("quota_filter=gemini,claude should hide gpt bucket, got: %q", outGC)
	}
	if !strings.Contains(outGC, "gem 5h") || !strings.Contains(outGC, "3p 5h") {
		t.Errorf("quota_filter=gemini,claude missing expected buckets, got: %q", outGC)
	}
}

func TestSystemMessageDoesNotPoisonStepDelta(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "transcript.jsonl")
	content := strings.Join([]string{
		`{"step_index":10,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-19T20:00:00Z","input_tokens":5000,"cache_read_tokens":50000,"output_tokens":500}`,
		`{"step_index":11,"type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-19T22:00:00Z"}`,
	}, "\n")
	if err := os.WriteFile(tr, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st := ConvState{ConversationID: "test", LastLoggedStep: -1}
	flushUnloggedSteps(dir, &st, tr, "gemini-4-neon-high", 0, false)

	if st.LastDurMs != 0 || st.SessDurMs != 0 {
		t.Fatalf("SYSTEM_MESSAGE 2h later must not poison duration, got dur=%d sessDur=%d", st.LastDurMs, st.SessDurMs)
	}
}

func TestPartialConfigPreservesDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGY_TELEMETRY_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"tps":"avg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig()
	if cfg.TPS != "avg" || !cfg.ShowQuota || !cfg.ShowCost || cfg.MaxLines != 2 || cfg.Cache != "call" {
		t.Fatalf("partial config did not preserve defaults: %+v", cfg)
	}
}

func TestParallelToolCallDuration(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "transcript.jsonl")
	content := strings.Join([]string{
		`{"step_index":68,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-19T23:59:01Z","input_tokens":10100,"cache_read_tokens":89500,"output_tokens":7275,"tool_calls":[{"name":"write_file"},{"name":"write_file"}]}`,
		`{"step_index":69,"type":"GENERIC","status":"DONE","created_at":"2026-11-19T23:59:08Z"}`,
		`{"step_index":70,"type":"GENERIC","status":"DONE","created_at":"2026-11-19T23:59:49Z"}`,
		`{"step_index":71,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T00:05:00Z"}`,
	}, "\n")
	if err := os.WriteFile(tr, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st := ConvState{ConversationID: "test", LastLoggedStep: -1}
	flushUnloggedSteps(dir, &st, tr, "gemini-4-neon-high", 0, false)

	if st.LastDurMs != 48000 {
		t.Fatalf("LastDurMs = %d, want 48000", st.LastDurMs)
	}
	if st.LastTPS < 151.0 || st.LastTPS > 152.0 {
		t.Fatalf("LastTPS = %.1f, want ~151.6", st.LastTPS)
	}
	if st.SessTimedOut != 7275 {
		t.Fatalf("SessTimedOut = %d, want 7275", st.SessTimedOut)
	}
}

func TestUserInputDoesNotPolluteDuration(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "transcript.jsonl")
	content := strings.Join([]string{
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-19T23:42:25Z","input_tokens":7899,"cache_read_tokens":0,"output_tokens":41}`,
		`{"step_index":2,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-19T23:43:34Z"}`,
	}, "\n")
	if err := os.WriteFile(tr, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st := ConvState{ConversationID: "test", LastLoggedStep: -1}
	flushUnloggedSteps(dir, &st, tr, "gemini-4-neon-high", 0, false)

	if st.LastDurMs != 0 || st.LastTPS != 0 || st.SessTimedOut != 0 {
		t.Fatalf("expected untimed step before USER_INPUT, got dur=%d tps=%.1f timedOut=%d", st.LastDurMs, st.LastTPS, st.SessTimedOut)
	}
}


func TestAdvisorBadgeShowsArmedOnFreshSession(t *testing.T) {
	advDir := t.TempDir()
	t.Setenv("AGY_ADVISOR_DIR", advDir)
	if err := os.WriteFile(filepath.Join(advDir, "config.json"), []byte(`{"enabled":true,"advisor_name":"Opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := formatAdvisorBadge("", 0)
	if !strings.Contains(got, "◎ Opus") {
		t.Fatalf("expected armed badge containing '◎ Opus' on fresh session, got %q", got)
	}

	// Disabled advisor must hide the badge.
	_ = os.WriteFile(filepath.Join(advDir, "config.json"), []byte(`{"enabled":false,"advisor_name":"Opus"}`), 0o644)
	if got := formatAdvisorBadge("conv-1", 0); got != "" {
		t.Fatalf("expected empty badge when disabled, got %q", got)
	}
}
