package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lunateque/agy-kit/internal/fsutil"
	"github.com/lunateque/agy-kit/internal/guard"
	"github.com/lunateque/agy-kit/internal/proc"
)

const (
	defaultTimeoutSec = 300
	maxTimeoutSec     = 900
	maxOutputBytes    = 16 * 1024 * 1024
	maxRetainedJobs   = 64
)

type AgentSpec struct {
	Backend    string `json:"backend"`
	Prompt     string `json:"prompt"`
	Model      string `json:"model"`
	CWD        string `json:"cwd"`
	SessionID  string `json:"session_id"`
	Thinking   string `json:"thinking"`
	ReadOnly   bool   `json:"read_only"`
	NoTools    bool   `json:"no_tools"`
	TimeoutSec int    `json:"timeout_sec"`
}

type UsageStats struct {
	InputTokens     int64   `json:"input_tokens,omitempty"`
	CacheReadTokens int64   `json:"cache_read_tokens,omitempty"`
	OutputTokens    int64   `json:"output_tokens,omitempty"`
	CostUSD         float64 `json:"cost_usd,omitempty"`
}

type AgentResult struct {
	Backend    string     `json:"backend"`
	Model      string     `json:"model,omitempty"`
	Provider   string     `json:"provider,omitempty"`
	SessionID  string     `json:"session_id,omitempty"`
	Response   string     `json:"response"`
	DurationMs int64      `json:"duration_ms"`
	Usage      UsageStats `json:"usage,omitempty"`
	Error      string     `json:"error,omitempty"`
}

func validateSpec(spec *AgentSpec) error {
	spec.Prompt = strings.TrimSpace(spec.Prompt)
	if spec.Prompt == "" {
		return errors.New("prompt is required")
	}
	spec.Backend = strings.ToLower(strings.TrimSpace(spec.Backend))
	if spec.Backend == "" {
		spec.Backend = "omp"
	}
	if spec.Backend != "omp" && spec.Backend != "claude" {
		return fmt.Errorf("unsupported backend %q (allowed: omp, claude)", spec.Backend)
	}

	spec.Model = strings.TrimSpace(spec.Model)
	spec.SessionID = strings.TrimSpace(spec.SessionID)
	spec.Thinking = strings.TrimSpace(spec.Thinking)
	if err := fsutil.ValidateSafeArg(spec.Model); err != nil {
		return fmt.Errorf("invalid model: %w", err)
	}
	if err := fsutil.ValidateSafeArg(spec.SessionID); err != nil {
		return fmt.Errorf("invalid session_id: %w", err)
	}
	if err := fsutil.ValidateSafeArg(spec.Thinking); err != nil {
		return fmt.Errorf("invalid thinking: %w", err)
	}
	if spec.Backend == "omp" && spec.Model == "" {
		return errors.New("model is required for omp backend (e.g. 'anthropic/claude-opus-5-5', 'openai-codex/gpt-6.2-sol', 'local-aigw/qwen3.8-flash-next') so omp never silently falls back to a blocked default provider")
	}
	if guard.IsBlockedGoogleModel(spec.Model) {
		return fmt.Errorf("refusing to invoke %q through external harness (Google consumer ToS ban risk)", spec.Model)
	}

	if spec.TimeoutSec <= 0 {
		spec.TimeoutSec = defaultTimeoutSec
	} else if spec.TimeoutSec > maxTimeoutSec {
		spec.TimeoutSec = maxTimeoutSec
	}

	if spec.CWD != "" {
		info, err := os.Stat(spec.CWD)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("invalid cwd %q", spec.CWD)
		}
	}
	return nil
}

func buildCommand(ctx context.Context, spec AgentSpec) (*exec.Cmd, error) {
	var bin string
	var args []string

	switch spec.Backend {
	case "omp":
		bin = "omp"
		args = append(args, "-p", "--mode", "json", "--allow-home")
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		if spec.SessionID != "" {
			args = append(args, "-r", spec.SessionID)
		}
		if spec.CWD != "" {
			args = append(args, "--cwd", spec.CWD)
		}
		if spec.Thinking != "" {
			args = append(args, "--thinking", spec.Thinking)
		}
		if spec.NoTools {
			args = append(args, "--no-tools")
		} else if spec.ReadOnly {
			args = append(args, "--tools", "read,grep,glob", "--no-extensions")
		}


	case "claude":
		bin = "claude"
		args = append(args, "-p", "--output-format", "json")
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		if spec.SessionID != "" {
			args = append(args, "--resume", spec.SessionID)
		}
		if spec.NoTools {
			args = append(args, "--tools", "")
		} else if spec.ReadOnly {
			args = append(args, "--permission-mode", "plan")
		}


	default:
		return nil, fmt.Errorf("unsupported backend %q", spec.Backend)
	}

	resolvedBin, err := proc.ResolveExecutable(bin)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, resolvedBin, args...)
	proc.Configure(cmd)
	// Deliver prompt via piped stdin (closes with EOF immediately; avoids Windows 32KB argv limit & cmd injection).
	cmd.Stdin = strings.NewReader(spec.Prompt)
	if spec.CWD != "" {
		cmd.Dir = spec.CWD
	}
	cmd.Env = append(os.Environ(), "AGY_BRIDGE_CHILD=1")
	return cmd, nil
}

func executeAgent(ctx context.Context, spec AgentSpec) (*AgentResult, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSec)*time.Second)
	defer cancel()

	cmd, err := buildCommand(runCtx, spec)
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	outW := &limitedWriter{buf: &stdout, limit: maxOutputBytes}
	errW := &limitedWriter{buf: &stderr, limit: 256 * 1024}
	cmd.Stdout = outW
	cmd.Stderr = errW

	start := time.Now()
	handle, startErr := proc.StartTracked(cmd)
	if startErr != nil {
		return nil, fmt.Errorf("failed to start %s: %w", spec.Backend, startErr)
	}
	defer handle.Close()

	runErr := cmd.Wait()
	elapsed := time.Since(start).Milliseconds()

	if outW.truncated {
		return &AgentResult{Backend: spec.Backend, DurationMs: elapsed},
			fmt.Errorf("%s stdout exceeded %d bytes limit (truncated)", spec.Backend, maxOutputBytes)
	}

	var res *AgentResult
	var parseErr error
	switch spec.Backend {
	case "omp":
		res, parseErr = parseOMPOutput(stdout.Bytes())
	case "claude":
		res, parseErr = parseClaudeOutput(stdout.Bytes())
	}

	if res == nil {
		res = &AgentResult{Backend: spec.Backend}
	}
	res.Backend = spec.Backend
	if res.DurationMs == 0 {
		res.DurationMs = elapsed
	}

	// Post-run ToS audit: reject if resumed session or alias resolved to a blocked Google provider.
	if guard.IsBlockedGoogleModel(res.Provider) || guard.IsBlockedGoogleModel(res.Model) {
		return nil, fmt.Errorf("blocked response from %s/%s (Google consumer OAuth is prohibited via external harness)", res.Provider, res.Model)
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%s timed out after %ds", spec.Backend, spec.TimeoutSec)
	}
	if runCtx.Err() == context.Canceled {
		return res, fmt.Errorf("%s cancelled", spec.Backend)
	}
	if runErr != nil {
		if res.Error != "" {
			return res, fmt.Errorf("%s error: %s", spec.Backend, res.Error)
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = runErr.Error()
		}
		if len(errMsg) > 1200 {
			errMsg = errMsg[:1200] + "..."
		}
		return res, fmt.Errorf("%s failed: %s", spec.Backend, errMsg)
	}
	if parseErr != nil {
		return res, fmt.Errorf("failed to parse %s output: %w", spec.Backend, parseErr)
	}
	if res.Error != "" {
		return res, fmt.Errorf("%s error: %s", spec.Backend, res.Error)
	}
	return res, nil
}

type limitedWriter struct {
	buf       *bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
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

// --- Output parsers ---

type ompEvent struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message *struct {
		Role         string `json:"role"`
		Model        string `json:"model"`
		Provider     string `json:"provider"`
		StopReason   string `json:"stopReason"`
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

func parseOMPOutput(raw []byte) (*AgentResult, error) {
	res := &AgentResult{Backend: "omp"}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), maxOutputBytes)

	foundEvent := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev ompEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		foundEvent = true

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
			res.Usage.InputTokens += ev.Message.Usage.Input
			res.Usage.CacheReadTokens += ev.Message.Usage.CacheRead
			res.Usage.OutputTokens += ev.Message.Usage.Output
			res.Usage.CostUSD += ev.Message.Usage.Cost.Total
			if ev.Message.ErrorMessage != "" {
				res.Error = ev.Message.ErrorMessage
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
	if !foundEvent {
		return res, errors.New("no JSON events in omp output")
	}
	return res, nil
}

type claudeModelUsageEntry struct {
	CostUSD      float64 `json:"costUSD"`
	OutputTokens int64   `json:"outputTokens"`
}

type claudeResultJSON struct {
	Type         string  `json:"type"`
	IsError      bool    `json:"is_error"`
	SessionID    string  `json:"session_id"`
	Result       string  `json:"result"`
	DurationMs   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens          int64 `json:"input_tokens"`
		OutputTokens         int64 `json:"output_tokens"`
		CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]claudeModelUsageEntry `json:"modelUsage"`
}

func parseClaudeOutput(raw []byte) (*AgentResult, error) {
	var payload []byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), maxOutputBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) > 0 && line[0] == '{' {
			cp := make([]byte, len(line))
			copy(cp, line)
			payload = cp
		}
	}
	if err := sc.Err(); err != nil {
		return &AgentResult{Backend: "claude"}, fmt.Errorf("claude output scan error: %w", err)
	}
	if len(payload) == 0 {
		return &AgentResult{Backend: "claude"}, errors.New("no JSON object in claude output")
	}

	var cr claudeResultJSON
	if err := json.Unmarshal(payload, &cr); err != nil {
		return &AgentResult{Backend: "claude"}, err
	}

	res := &AgentResult{
		Backend:    "claude",
		Provider:   "anthropic",
		SessionID:  cr.SessionID,
		DurationMs: cr.DurationMs,
		Usage: UsageStats{
			InputTokens:     cr.Usage.InputTokens,
			CacheReadTokens: cr.Usage.CacheReadInputTokens,
			OutputTokens:    cr.Usage.OutputTokens,
			CostUSD:         cr.TotalCostUSD,
		},
	}

	// Deterministic model selection: prefer highest output tokens, then lexicographical.
	if len(cr.ModelUsage) > 0 {
		names := make([]string, 0, len(cr.ModelUsage))
		for m := range cr.ModelUsage {
			names = append(names, m)
		}
		sort.Slice(names, func(i, j int) bool {
			ei, ej := cr.ModelUsage[names[i]], cr.ModelUsage[names[j]]
			if ei.OutputTokens != ej.OutputTokens {
				return ei.OutputTokens > ej.OutputTokens
			}
			return names[i] < names[j]
		})
		res.Model = names[0]
	}

	if cr.IsError {
		res.Error = cr.Result
	} else {
		res.Response = strings.TrimSpace(cr.Result)
	}
	return res, nil
}

// --- Job manager (tracks both sync and async runs for clean shutdown) ---

type jobSummary struct {
	JobID     string       `json:"job_id"`
	Status    string       `json:"status"` // running | done | error | cancelled
	Backend   string       `json:"backend"`
	Model     string       `json:"model,omitempty"`
	CWD       string       `json:"cwd,omitempty"`
	StartedAt string       `json:"started_at"`
	ElapsedMs int64        `json:"elapsed_ms"`
	Result    *AgentResult `json:"result,omitempty"`
	Error     string       `json:"error,omitempty"`
}

type job struct {
	id        string
	spec      AgentSpec
	startedAt time.Time
	doneAt    time.Time
	status    string
	res       *AgentResult
	errMsg    string
	cancel    context.CancelFunc
	doneCh    chan struct{}
}

type jobManager struct {
	mu       sync.Mutex
	seq      atomic.Uint64
	jobs     map[string]*job
	order    []string
	activeWG sync.WaitGroup
	exec     func(context.Context, AgentSpec) (*AgentResult, error)
}

func newJobManager() *jobManager {
	return &jobManager{
		jobs: make(map[string]*job),
		exec: executeAgent,
	}
}

func (m *jobManager) runSync(ctx context.Context, spec AgentSpec) (*AgentResult, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	id := fmt.Sprintf("sync-%d", m.seq.Add(1))
	j := &job{
		id:        id,
		spec:      spec,
		startedAt: time.Now(),
		status:    "running",
		cancel:    cancel,
		doneCh:    make(chan struct{}),
	}

	m.mu.Lock()
	m.jobs[id] = j
	m.order = append(m.order, id)
	m.pruneLocked()
	m.activeWG.Add(1)
	m.mu.Unlock()

	defer m.activeWG.Done()
	defer close(j.doneCh)

	res, err := m.exec(runCtx, spec)

	m.mu.Lock()
	j.doneAt = time.Now()
	j.res = res
	if j.status == "cancelled" {
		// preserve cancelled state
	} else if err != nil {
		j.status = "error"
		j.errMsg = err.Error()
	} else {
		j.status = "done"
	}
	m.mu.Unlock()

	return res, err
}

func (m *jobManager) spawn(parentCtx context.Context, spec AgentSpec) (map[string]any, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}

	id := fmt.Sprintf("job-%d", m.seq.Add(1))
	jobCtx, cancel := context.WithCancel(parentCtx)
	j := &job{
		id:        id,
		spec:      spec,
		startedAt: time.Now(),
		status:    "running",
		cancel:    cancel,
		doneCh:    make(chan struct{}),
	}

	m.mu.Lock()
	m.jobs[id] = j
	m.order = append(m.order, id)
	m.pruneLocked()
	m.activeWG.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.activeWG.Done()
		defer close(j.doneCh)
		defer cancel()
		res, err := m.exec(jobCtx, spec)

		m.mu.Lock()
		j.doneAt = time.Now()
		j.res = res
		if j.status == "cancelled" {
			// preserve cancelled state
		} else if err != nil {
			j.status = "error"
			j.errMsg = err.Error()
		} else {
			j.status = "done"
		}
		m.mu.Unlock()
	}()

	cwdDisplay := spec.CWD
	if cwdDisplay == "" {
		cwdDisplay, _ = filepath.Abs(".")
	}
	return map[string]any{
		"job_id":  id,
		"status":  "running",
		"backend": spec.Backend,
		"model":   spec.Model,
		"cwd":     cwdDisplay,
	}, nil
}

func (m *jobManager) pruneLocked() {
	if len(m.order) <= maxRetainedJobs {
		return
	}
	kept := m.order[:0]
	excess := len(m.order) - maxRetainedJobs
	for _, id := range m.order {
		j, ok := m.jobs[id]
		if !ok {
			continue
		}
		if excess > 0 && j.status != "running" {
			delete(m.jobs, id)
			excess--
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
}

func (m *jobManager) snapshot(j *job) jobSummary {
	end := j.doneAt
	if end.IsZero() {
		end = time.Now()
	}
	model := j.spec.Model
	if j.res != nil && j.res.Model != "" {
		model = j.res.Model
	}
	return jobSummary{
		JobID:     j.id,
		Status:    j.status,
		Backend:   j.spec.Backend,
		Model:     model,
		CWD:       j.spec.CWD,
		StartedAt: j.startedAt.Format(time.RFC3339),
		ElapsedMs: end.Sub(j.startedAt).Milliseconds(),
		Result:    j.res,
		Error:     j.errMsg,
	}
}

func (m *jobManager) wait(ctx context.Context, ids []string, timeoutSec int) (map[string]any, error) {
	if len(ids) == 0 {
		return nil, errors.New("job_id or job_ids is required")
	}
	if timeoutSec <= 0 {
		timeoutSec = 60
	} else if timeoutSec > maxTimeoutSec {
		timeoutSec = maxTimeoutSec
	}

	m.mu.Lock()
	targets := make([]*job, 0, len(ids))
	for _, id := range ids {
		j, ok := m.jobs[id]
		if !ok {
			m.mu.Unlock()
			return nil, fmt.Errorf("unknown job_id %q", id)
		}
		targets = append(targets, j)
	}
	m.mu.Unlock()

	timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer timer.Stop()

	for _, j := range targets {
		select {
		case <-j.doneCh:
		case <-timer.C:
			goto done
		case <-ctx.Done():
			goto done
		}
	}

done:
	m.mu.Lock()
	out := make([]jobSummary, 0, len(targets))
	allDone := true
	for _, j := range targets {
		s := m.snapshot(j)
		if s.Status == "running" {
			allDone = false
		}
		out = append(out, s)
	}
	m.mu.Unlock()

	return map[string]any{
		"all_done": allDone,
		"jobs":     out,
	}, nil
}

func (m *jobManager) cancel(id string) bool {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	if j.status == "running" {
		j.status = "cancelled"
		j.errMsg = "cancelled by caller"
		j.cancel()
	}
	m.mu.Unlock()
	return true
}

func (m *jobManager) cancelAllAndWait(timeout time.Duration) {
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.status == "running" {
			j.status = "cancelled"
			j.errMsg = "server shutting down"
			j.cancel()
		}
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (m *jobManager) list() []jobSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]jobSummary, 0, len(m.order))
	for _, id := range m.order {
		if j, ok := m.jobs[id]; ok {
			out = append(out, m.snapshot(j))
		}
	}
	return out
}
