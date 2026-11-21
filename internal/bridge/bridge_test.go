package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lunateque/agy-kit/internal/proc"
)

func TestValidateSpecGuardrails(t *testing.T) {
	// Empty model on omp must fail (prevents silent fallback to blocked default).
	if err := validateSpec(&AgentSpec{Backend: "omp", Prompt: "hi"}); err == nil {
		t.Fatal("expected error when model is empty for omp")
	}

	// Antigravity & Gemini consumer models must be blocked.
	for _, m := range []string{
		"google-antigravity/gemini-4-neon-high",
		"google-gemini-cli/gemini-3.8-flash",
		"gemini-4-neon-high",
		"gemini-4-hydrogen-high",
		"gemini-4-argon-high",
	} {
		err := validateSpec(&AgentSpec{Backend: "omp", Prompt: "hi", Model: m})
		if err == nil {
			t.Fatalf("expected error blocking model %q", m)
		}
	}

	// Valid models pass.
	if err := validateSpec(&AgentSpec{Backend: "omp", Prompt: "hi", Model: "anthropic/claude-opus-5-5"}); err != nil {
		t.Fatalf("unexpected error for valid omp model: %v", err)
	}
	if err := validateSpec(&AgentSpec{Backend: "claude", Prompt: "hi"}); err != nil {
		t.Fatalf("unexpected error for claude backend: %v", err)
	}
}

func TestBuildCommandSendsPromptViaStdin(t *testing.T) {
	spec := AgentSpec{
		Backend: "omp",
		Model:   "anthropic/claude-opus-5-5",
		Prompt:  "--model=google-antigravity/x --tools=bash",
	}
	cmd, err := buildCommand(context.Background(), spec)
	if err != nil {
		t.Fatalf("buildCommand failed: %v", err)
	}
	// Prompt must NEVER appear in argv (prevents Windows 32KB limit & cmd injection).
	for _, a := range cmd.Args {
		if a == spec.Prompt {
			t.Fatalf("prompt leaked into argv: %q", cmd.Args)
		}
	}
	if cmd.Stdin == nil {
		t.Fatal("expected prompt on stdin, got nil")
	}
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil || string(got) != spec.Prompt {
		t.Fatalf("expected stdin prompt %q, got %q (err=%v)", spec.Prompt, string(got), err)
	}
}

func TestParseOMPOutput(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"session","version":3,"id":"01a1f661-test","cwd":"C:\\tmp"}`,
		`{"type":"message_end","message":{"role":"assistant","provider":"local-aigw","model":"qwen3.8-flash-next","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"step 1"}],"usage":{"input":1000,"output":50,"cacheRead":200,"cost":{"total":0.01}}}}`,
		`{"type":"message_end","message":{"role":"assistant","provider":"local-aigw","model":"qwen3.8-flash-next","content":[{"type":"text","text":"final answer"}],"usage":{"input":1200,"output":80,"cacheRead":900,"cost":{"total":0.02}}}}`,
	}, "\n")

	res, err := parseOMPOutput([]byte(ndjson))
	if err != nil {
		t.Fatalf("parseOMPOutput failed: %v", err)
	}
	if res.SessionID != "01a1f661-test" || res.Response != "final answer" {
		t.Fatalf("unexpected parsed result: %+v", res)
	}
	if res.Usage.OutputTokens != 130 || res.Usage.CacheReadTokens != 1100 {
		t.Fatalf("unexpected usage: %+v", res.Usage)
	}
}

func TestParseClaudeOutputDeterministicModel(t *testing.T) {
	raw := "Warning: something on line 1\n" +
		`{"type":"result","is_error":false,"session_id":"sess-42","result":"LGTM","duration_ms":1234,"total_cost_usd":0.05,"usage":{"input_tokens":500,"output_tokens":40,"cache_read_input_tokens":300},"modelUsage":{"claude-haiku-4-5":{"outputTokens":10},"claude-opus-5-5":{"outputTokens":400}}}` + "\n"

	res, err := parseClaudeOutput([]byte(raw))
	if err != nil {
		t.Fatalf("parseClaudeOutput failed: %v", err)
	}
	if res.SessionID != "sess-42" || res.Response != "LGTM" || res.Model != "claude-opus-5-5" {
		t.Fatalf("unexpected parsed result: %+v", res)
	}
}

func TestMCPProtocolToolCallAndCancel(t *testing.T) {
	mgr := newJobManager()
	started := make(chan struct{}, 1)
	mgr.exec = func(ctx context.Context, spec AgentSpec) (*AgentResult, error) {
		if spec.Prompt == "slow" {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &AgentResult{
			Backend:   spec.Backend,
			Model:     spec.Model,
			SessionID: "mock-sess",
			Response:  "echo: " + spec.Prompt,
		}, nil
	}

	var out bytes.Buffer
	srv := newMCPServer(mgr, &out)
	ctx := context.Background()

	// 1. Parse error must return -32700 with id:null
	srv.handleLine(ctx, []byte(`{not valid json`))

	// 2. Initialize negotiates supported protocol version
	srv.handleLine(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2099-01-01"}}`))

	// 3. tools/call happy path + isError path
	srv.handleLine(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run_agent","arguments":{"backend":"omp","model":"anthropic/claude-opus-5-5","prompt":"hi"}}}`))
	srv.handleLine(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"run_agent","arguments":{"backend":"omp","model":"google-antigravity/neon","prompt":"hi"}}}`))

	// 4. notifications/cancelled aborts in-flight run_agent
	srv.handleLine(ctx, []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"run_agent","arguments":{"backend":"omp","model":"anthropic/claude-opus-5-5","prompt":"slow"}}}`))
	<-started
	srv.handleLine(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":4}}`))

	srv.waitInflight()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 RPC responses, got %d:\n%s", len(lines), out.String())
	}

	byID := map[string]rpcResponse{}
	for _, l := range lines {
		var r rpcResponse
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("invalid JSON-RPC line %q: %v", l, err)
		}
		byID[canonicalID(r.ID)] = r
	}

	if byID["null"].Error == nil || byID["null"].Error.Code != -32700 {
		t.Fatalf("expected -32700 parse error, got %+v", byID["null"])
	}
	initRes, _ := json.Marshal(byID["1"].Result)
	if !strings.Contains(string(initRes), `"2025-03-26"`) {
		t.Fatalf("expected fallback protocolVersion 2025-03-26, got %s", initRes)
	}
	call2, _ := json.Marshal(byID["2"].Result)
	if !strings.Contains(string(call2), "echo: hi") {
		t.Fatalf("expected echo response for id=2, got %s", call2)
	}
	call3, _ := json.Marshal(byID["3"].Result)
	if !strings.Contains(string(call3), `"isError":true`) {
		t.Fatalf("expected isError=true for blocked model id=3, got %s", call3)
	}
	call4, _ := json.Marshal(byID["4"].Result)
	if !strings.Contains(string(call4), `"isError":true`) {
		t.Fatalf("expected cancelled run_agent id=4 to return isError=true, got %s", call4)
	}
}

func TestRecursionGuardRefusesChild(t *testing.T) {
	t.Setenv("AGY_BRIDGE_CHILD", "1")
	var out bytes.Buffer
	srv := newMCPServer(newJobManager(), &out)
	srv.handleLine(context.Background(), []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"list_jobs","arguments":{}}}`))
	srv.waitInflight()

	if !strings.Contains(out.String(), "recursive") || !strings.Contains(out.String(), `"isError":true`) {
		t.Fatalf("expected recursion guard error, got: %s", out.String())
	}
}

func TestTrackedProcessKillsOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", "ping -n 10 127.0.0.1 > NUL")
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", "sleep 10")
	}
	proc.Configure(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	start := time.Now()
	handle, err := proc.StartTracked(cmd)
	if err != nil {
		t.Fatalf("StartTracked failed: %v", err)
	}
	defer handle.Close()

	_ = cmd.Wait()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("tracked process did not terminate promptly on cancel: took %v", elapsed)
	}
}
