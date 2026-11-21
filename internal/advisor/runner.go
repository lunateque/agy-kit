package advisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lunateque/agy-kit/internal/fsutil"
	"github.com/lunateque/agy-kit/internal/guard"
	"github.com/lunateque/agy-kit/internal/proc"
)

const maxAdvisorOutputBytes = 8 * 1024 * 1024

// RunRequest describes one persistent-session advisor invocation.
type RunRequest struct {
	Backend          string
	Model            string
	Thinking         string
	ScopeDir         string
	SessionDir       string
	SystemPromptPath string
	HasSession       bool
	AdvisorSessionID string
	Prompt           string
	TimeoutSec       int
}

// RunResult returns the advisor's response and token/cache telemetry.
type RunResult struct {
	Response        string
	SessionID       string
	Model           string
	Provider        string
	InputTokens     int64
	CacheReadTokens int64
	OutputTokens    int64
	CostUSD         float64
	DurationMs      int64
	QuotaExceeded   bool
	RawStderr       string
}

type RunnerFunc func(ctx context.Context, req RunRequest) (*RunResult, error)

// ExecuteAdvisor runs omp (default, Claude subscription via auth-broker) or
// claude CLI with persistent session caching and delta delivered via stdin.
func ExecuteAdvisor(ctx context.Context, req RunRequest) (*RunResult, error) {
	if err := fsutil.ValidateSafeArg(req.Model); err != nil {
		return nil, fmt.Errorf("invalid advisor model: %w", err)
	}
	if err := fsutil.ValidateSafeArg(req.Thinking); err != nil {
		return nil, fmt.Errorf("invalid advisor thinking level: %w", err)
	}
	if err := fsutil.ValidateSafeArg(req.AdvisorSessionID); err != nil {
		return nil, fmt.Errorf("invalid advisor session id: %w", err)
	}
	if guard.IsBlockedGoogleModel(req.Model) {
		return nil, fmt.Errorf("blocked google consumer model %q in advisor", req.Model)
	}
	if strings.TrimSpace(req.ScopeDir) == "" {
		return nil, errors.New("refusing to run advisor without a concrete non-home scope directory")
	}
	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 120
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second)
	defer cancel()

	switch req.Backend {
	case "omp":
		return runOMPAdvisor(runCtx, req)
	case "claude":
		return runClaudeAdvisor(runCtx, req)
	default:
		return nil, fmt.Errorf("unsupported advisor backend %q", req.Backend)
	}
}

func runOMPAdvisor(ctx context.Context, req RunRequest) (*RunResult, error) {
	bin, err := proc.ResolveExecutable("omp")
	if err != nil {
		return nil, fmt.Errorf("omp unavailable: %w", err)
	}
	if err := os.MkdirAll(req.SessionDir, 0o755); err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--mode", "json",
		"--model", req.Model,
		"--thinking", req.Thinking,
		"--tools", "read,grep,glob",
		"--system-prompt", req.SystemPromptPath,
		"--session-dir", req.SessionDir,
		"--no-extensions",
		"--no-skills",
		"--no-rules",
		"--no-lsp",
		"--no-title",
		"--no-judge",
		"--no-spotlight",
		"--no-context-files",
		"--cwd", req.ScopeDir,
	}
	if req.HasSession {
		args = append(args, "-c")
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	proc.Configure(cmd)
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Dir = req.ScopeDir
	cmd.Env = append(os.Environ(), "AGY_ADVISOR_CHILD=1", "AGY_BRIDGE_CHILD=1")

	var stdout, stderr bytes.Buffer
	outW := &limitedBuf{buf: &stdout, limit: maxAdvisorOutputBytes}
	errW := &limitedBuf{buf: &stderr, limit: 128 * 1024}
	cmd.Stdout = outW
	cmd.Stderr = errW

	start := time.Now()
	handle, err := proc.StartTracked(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start omp: %w", err)
	}
	defer handle.Close()

	runErr := cmd.Wait()
	elapsed := time.Since(start).Milliseconds()

	if outW.truncated {
		return &RunResult{DurationMs: elapsed}, fmt.Errorf("omp advisor stdout exceeded %d bytes (truncated)", maxAdvisorOutputBytes)
	}

	res, parseErr := parseOMPJSON(stdout.Bytes())
	if res == nil {
		res = &RunResult{}
	}
	res.DurationMs = elapsed
	res.RawStderr = trimCapUTF8(strings.TrimSpace(stderr.String()), 800)

	if guard.IsBlockedGoogleModel(res.Provider) || guard.IsBlockedGoogleModel(res.Model) {
		return nil, fmt.Errorf("blocked google consumer provider %s/%s in advisor output", res.Provider, res.Model)
	}

	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("omp advisor timed out after %ds", req.TimeoutSec)
	}
	if runErr != nil {
		msg := res.RawStderr
		if msg == "" {
			msg = runErr.Error()
		}
		if isQuotaError(msg) {
			res.QuotaExceeded = true
		}
		return res, fmt.Errorf("omp advisor failed: %s", msg)
	}
	if parseErr != nil {
		if isQuotaError(res.RawStderr) {
			res.QuotaExceeded = true
		}
		return res, parseErr
	}
	return res, nil
}

func runClaudeAdvisor(ctx context.Context, req RunRequest) (*RunResult, error) {
	bin, err := proc.ResolveExecutable("claude")
	if err != nil {
		return nil, fmt.Errorf("claude unavailable: %w", err)
	}
	sessID := req.AdvisorSessionID
	if sessID == "" {
		sessID = newUUIDv4()
	}

	invoke := func(resume bool) (*RunResult, error) {
		args := []string{
			"-p",
			"--output-format", "json",
			"--model", req.Model,
			"--effort", req.Thinking,
			"--tools", "Read,Grep,Glob",
			"--permission-mode", "plan",
			"--system-prompt-file", req.SystemPromptPath,
		}
		if resume {
			args = append(args, "--resume", sessID)
		} else {
			args = append(args, "--session-id", sessID)
		}

		cmd := exec.CommandContext(ctx, bin, args...)
		proc.Configure(cmd)
		cmd.Stdin = strings.NewReader(req.Prompt)
		cmd.Dir = req.ScopeDir
		cmd.Env = append(os.Environ(), "AGY_ADVISOR_CHILD=1", "AGY_BRIDGE_CHILD=1")

		var stdout, stderr bytes.Buffer
		outW := &limitedBuf{buf: &stdout, limit: maxAdvisorOutputBytes}
		errW := &limitedBuf{buf: &stderr, limit: 128 * 1024}
		cmd.Stdout = outW
		cmd.Stderr = errW

		start := time.Now()
		handle, err := proc.StartTracked(cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to start claude: %w", err)
		}
		defer handle.Close()

		runErr := cmd.Wait()
		elapsed := time.Since(start).Milliseconds()

		if outW.truncated {
			return &RunResult{DurationMs: elapsed}, fmt.Errorf("claude advisor stdout exceeded %d bytes (truncated)", maxAdvisorOutputBytes)
		}

		res, parseErr := parseClaudeJSON(stdout.Bytes())
		if res == nil {
			res = &RunResult{}
		}
		if res.SessionID == "" {
			res.SessionID = sessID
		}
		res.DurationMs = elapsed
		res.RawStderr = trimCapUTF8(strings.TrimSpace(stderr.String()), 800)

		if ctx.Err() == context.DeadlineExceeded {
			return res, fmt.Errorf("claude advisor timed out after %ds", req.TimeoutSec)
		}
		if runErr != nil {
			msg := res.RawStderr
			if msg == "" {
				msg = runErr.Error()
			}
			if isQuotaError(msg) {
				res.QuotaExceeded = true
			}
			return res, fmt.Errorf("claude advisor failed: %s", msg)
		}
		if parseErr != nil {
			return res, parseErr
		}
		return res, nil
	}

	res, err := invoke(req.HasSession)
	if err != nil && !req.HasSession && strings.Contains(strings.ToLower(err.Error()), "already in use") {
		return invoke(true)
	}
	return res, err
}

func isQuotaError(s string) bool {
	l := strings.ToLower(s)
	for _, pat := range []string{"rate_limit", "rate limit", "quota", "usage limit", "429", "overloaded", "credit balance"} {
		if strings.Contains(l, pat) {
			return true
		}
	}
	return false
}

type limitedBuf struct {
	buf       *bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedBuf) Write(p []byte) (int, error) {
	rem := w.limit - w.buf.Len()
	if rem > 0 {
		if len(p) <= rem {
			_, _ = w.buf.Write(p)
		} else {
			_, _ = w.buf.Write(p[:rem])
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

type ompStreamEvent struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message *struct {
		Role         string `json:"role"`
		Model        string `json:"model"`
		Provider     string `json:"provider"`
		ErrorMessage string `json:"errorMessage"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			CacheRead int64 `json:"cacheRead"`
			Cost      struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

func parseOMPJSON(raw []byte) (*RunResult, error) {
	res := &RunResult{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), maxAdvisorOutputBytes)
	found := false
	var lastErrMsg string

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev ompStreamEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		found = true
		switch ev.Type {
		case "session":
			if ev.ID != "" {
				res.SessionID = ev.ID
			}
		case "message_end":
			if ev.Message == nil || ev.Message.Role != "assistant" {
				continue
			}
			if ev.Message.Model != "" {
				res.Model = ev.Message.Model
			}
			if ev.Message.Provider != "" {
				res.Provider = ev.Message.Provider
			}
			res.InputTokens += ev.Message.Usage.Input
			res.CacheReadTokens += ev.Message.Usage.CacheRead
			res.OutputTokens += ev.Message.Usage.Output
			res.CostUSD += ev.Message.Usage.Cost.Total
			// Track error state of the LATEST assistant turn: a successful retry clears an earlier transient error!
			if ev.Message.ErrorMessage != "" {
				lastErrMsg = ev.Message.ErrorMessage
			} else {
				lastErrMsg = ""
			}
			var texts []string
			for _, c := range ev.Message.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					texts = append(texts, c.Text)
				}
			}
			if len(texts) > 0 {
				res.Response = strings.TrimSpace(strings.Join(texts, "\n"))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("omp output scan error: %w", err)
	}
	if lastErrMsg != "" {
		if isQuotaError(lastErrMsg) {
			res.QuotaExceeded = true
		}
		return res, errors.New(lastErrMsg)
	}
	if !found {
		return res, errors.New("no JSON events in omp advisor output")
	}
	return res, nil
}

type claudeOutJSON struct {
	IsError      bool    `json:"is_error"`
	SessionID    string  `json:"session_id"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens          int64 `json:"input_tokens"`
		OutputTokens         int64 `json:"output_tokens"`
		CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		OutputTokens int64 `json:"outputTokens"`
	} `json:"modelUsage"`
}

func parseClaudeJSON(raw []byte) (*RunResult, error) {
	var payload []byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), maxAdvisorOutputBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) > 0 && line[0] == '{' {
			cp := make([]byte, len(line))
			copy(cp, line)
			payload = cp
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("claude output scan error: %w", err)
	}
	if len(payload) == 0 {
		return nil, errors.New("no JSON object in claude advisor output")
	}
	var cr claudeOutJSON
	if err := json.Unmarshal(payload, &cr); err != nil {
		return nil, err
	}
	res := &RunResult{
		Provider:        "anthropic",
		SessionID:       cr.SessionID,
		InputTokens:     cr.Usage.InputTokens,
		CacheReadTokens: cr.Usage.CacheReadInputTokens,
		OutputTokens:    cr.Usage.OutputTokens,
		CostUSD:         cr.TotalCostUSD,
	}
	if len(cr.ModelUsage) > 0 {
		names := make([]string, 0, len(cr.ModelUsage))
		for m := range cr.ModelUsage {
			names = append(names, m)
		}
		sort.Slice(names, func(i, j int) bool {
			return cr.ModelUsage[names[i]].OutputTokens > cr.ModelUsage[names[j]].OutputTokens
		})
		res.Model = names[0]
	}
	if cr.IsError {
		if isQuotaError(cr.Result) {
			res.QuotaExceeded = true
		}
		return res, errors.New(cr.Result)
	}
	res.Response = strings.TrimSpace(cr.Result)
	return res, nil
}

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// CheckBackendAvailable probes whether the configured backend binary exists and
// is a safe native executable (not a .cmd/.bat shim).
func CheckBackendAvailable(backend string) (string, error) {
	bin := "omp"
	if backend == "claude" {
		bin = "claude"
	}
	p, err := proc.ResolveExecutable(bin)
	if err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}
