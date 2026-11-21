package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

const (
	serverName     = "agy-bridge"
	serverVersion  = "0.1.0"
	maxRPCReqBytes = 16 * 1024 * 1024
)

var supportedProtos = map[string]bool{
	"2025-03-26": true,
	"2024-11-05": true,
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

type mcpServer struct {
	mgr        *jobManager
	out        io.Writer
	mu         sync.Mutex
	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc
	wg         sync.WaitGroup
}

func newMCPServer(mgr *jobManager, out io.Writer) *mcpServer {
	return &mcpServer{
		mgr:      mgr,
		out:      out,
		inflight: make(map[string]context.CancelFunc),
	}
}

func (s *mcpServer) writeRaw(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(data, '\n'))
}

func (s *mcpServer) writeResp(resp rpcResponse) {
	if len(resp.ID) == 0 {
		resp.ID = json.RawMessage("null")
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	s.writeRaw(data)
}

func (s *mcpServer) replyResult(id json.RawMessage, result any) {
	s.writeResp(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *mcpServer) replyError(id json.RawMessage, code int, msg string) {
	s.writeResp(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}

func canonicalID(raw json.RawMessage) string {
	return string(bytes.TrimSpace(raw))
}

func agentParamProps() map[string]any {
	return map[string]any{
		"backend": map[string]any{
			"type":        "string",
			"enum":        []string{"omp", "claude"},
			"description": "External agent CLI to invoke. Default: omp.",
		},
		"prompt": map[string]any{
			"type":        "string",
			"description": "Task or question for the external agent.",
		},
		"model": map[string]any{
			"type":        "string",
			"description": "Model identifier. Required for omp (e.g. 'anthropic/claude-opus-5-5', 'openai-codex/gpt-6.2-sol', 'local-aigw/qwen3.8-flash-next'). Optional for claude (e.g. 'opus', 'sonnet'). Google Antigravity/Gemini CLI models are blocked.",
		},
		"cwd": map[string]any{
			"type":        "string",
			"description": "Working directory for the agent process.",
		},
		"session_id": map[string]any{
			"type":        "string",
			"description": "Optional session ID from a previous call to continue that conversation.",
		},
		"thinking": map[string]any{
			"type":        "string",
			"enum":        []string{"off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"},
			"description": "Thinking effort level (omp backend).",
		},
		"read_only": map[string]any{
			"type":        "boolean",
			"description": "Restrict agent to read-only inspection (omp: read/grep/find tools; claude: plan permission mode).",
		},
		"no_tools": map[string]any{
			"type":        "boolean",
			"description": "Disable tools for pure text reasoning/review.",
		},
		"timeout_sec": map[string]any{
			"type":        "integer",
			"description": "Execution timeout in seconds (default 300, max 900).",
		},
	}
}

func (s *mcpServer) toolDefs() []mcpTool {
	return []mcpTool{
		{
			Name:        "run_agent",
			Description: "Run an external omp or claude agent synchronously and return its final response, session_id (for follow-ups), and token/timing stats.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": agentParamProps(),
				"required":   []string{"prompt"},
			},
		},
		{
			Name:        "spawn_agent",
			Description: "Launch an external omp or claude agent asynchronously and return a job_id immediately so multiple agents can run in parallel.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": agentParamProps(),
				"required":   []string{"prompt"},
			},
		},
		{
			Name:        "wait_agent",
			Description: "Wait for one or more spawned agent jobs to complete (or until timeout_sec) and return their status and outputs.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "Single job_id returned by spawn_agent.",
					},
					"job_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Multiple job_ids to wait on together.",
					},
					"timeout_sec": map[string]any{
						"type":        "integer",
						"description": "Max seconds to wait before returning current status (default 60, max 900).",
					},
				},
			},
		},
		{
			Name:        "list_jobs",
			Description: "List bridge jobs (running and finished), or cancel a running job by cancel_job_id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cancel_job_id": map[string]any{
						"type":        "string",
						"description": "Optional job_id to cancel and terminate.",
					},
				},
			},
		},
	}
}

func (s *mcpServer) handleLine(ctx context.Context, line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}

	// Support JSON-RPC 2.0 batch arrays (required by MCP 2025-03-26).
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
			s.replyError(json.RawMessage("null"), -32600, "invalid batch request")
			return
		}
		for _, item := range batch {
			s.handleSingle(ctx, item)
		}
		return
	}

	s.handleSingle(ctx, trimmed)
}

func (s *mcpServer) handleSingle(ctx context.Context, raw []byte) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.replyError(json.RawMessage("null"), -32700, "parse error: "+err.Error())
		return
	}
	isNotification := len(req.ID) == 0 || canonicalID(req.ID) == "null"

	switch req.Method {
	case "initialize":
		proto := "2025-03-26"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && supportedProtos[p.ProtocolVersion] {
			proto = p.ProtocolVersion
		}
		s.replyResult(req.ID, map[string]any{
			"protocolVersion": proto,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    serverName,
				"version": serverVersion,
			},
		})

	case "notifications/initialized":
		return

	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &p) == nil && len(p.RequestID) > 0 {
			key := canonicalID(p.RequestID)
			s.inflightMu.Lock()
			if cancel, ok := s.inflight[key]; ok {
				cancel()
			}
			s.inflightMu.Unlock()
		}
		return

	case "ping":
		if !isNotification {
			s.replyResult(req.ID, map[string]any{})
		}

	case "tools/list":
		s.replyResult(req.ID, map[string]any{
			"tools": s.toolDefs(),
		})

	case "tools/call":
		reqCtx, reqCancel := context.WithCancel(ctx)
		key := canonicalID(req.ID)
		s.inflightMu.Lock()
		s.inflight[key] = reqCancel
		s.inflightMu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer reqCancel()
			defer func() {
				s.inflightMu.Lock()
				delete(s.inflight, key)
				s.inflightMu.Unlock()
			}()
			s.handleToolCall(reqCtx, ctx, req.ID, req.Params)
		}()

	default:
		if !isNotification {
			s.replyError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *mcpServer) handleToolCall(reqCtx, serverCtx context.Context, id json.RawMessage, rawParams json.RawMessage) {
	if strings.TrimSpace(os.Getenv("AGY_BRIDGE_CHILD")) == "1" {
		s.replyResult(id, mcpToolResult{
			Content: []mcpToolContent{{Type: "text", Text: "refusing recursive agy-bridge call inside child agent"}},
			IsError: true,
		})
		return
	}

	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &p); err != nil {
		s.replyError(id, -32602, "invalid params: "+err.Error())
		return
	}

	var out any
	var toolErr error

	switch p.Name {
	case "run_agent":
		var spec AgentSpec
		if err := json.Unmarshal(p.Arguments, &spec); err != nil {
			toolErr = fmt.Errorf("invalid arguments: %w", err)
		} else {
			out, toolErr = s.mgr.runSync(reqCtx, spec)
		}

	case "spawn_agent":
		var spec AgentSpec
		if err := json.Unmarshal(p.Arguments, &spec); err != nil {
			toolErr = fmt.Errorf("invalid arguments: %w", err)
		} else {
			// Async jobs live on serverCtx (not reqCtx) so they outlive the spawn RPC.
			out, toolErr = s.mgr.spawn(serverCtx, spec)
		}

	case "wait_agent":
		var args struct {
			JobID      string   `json:"job_id"`
			JobIDs     []string `json:"job_ids"`
			TimeoutSec int      `json:"timeout_sec"`
		}
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			toolErr = fmt.Errorf("invalid arguments: %w", err)
		} else {
			ids := args.JobIDs
			if args.JobID != "" {
				ids = append([]string{args.JobID}, ids...)
			}
			out, toolErr = s.mgr.wait(reqCtx, ids, args.TimeoutSec)
		}

	case "list_jobs":
		var args struct {
			CancelJobID string `json:"cancel_job_id"`
		}
		_ = json.Unmarshal(p.Arguments, &args)
		if args.CancelJobID != "" {
			ok := s.mgr.cancel(args.CancelJobID)
			out = map[string]any{"job_id": args.CancelJobID, "cancelled": ok}
		} else {
			out = map[string]any{"jobs": s.mgr.list()}
		}

	default:
		s.replyError(id, -32602, "unknown tool: "+p.Name)
		return
	}

	if toolErr != nil {
		s.replyResult(id, mcpToolResult{
			Content: []mcpToolContent{{Type: "text", Text: toolErr.Error()}},
			IsError: true,
		})
		return
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		s.replyResult(id, mcpToolResult{
			Content: []mcpToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		})
		return
	}
	s.replyResult(id, mcpToolResult{
		Content: []mcpToolContent{{Type: "text", Text: string(b)}},
	})
}

func (s *mcpServer) serve(ctx context.Context, in io.Reader) error {
	br := bufio.NewReaderSize(in, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > maxRPCReqBytes {
			s.replyError(json.RawMessage("null"), -32600, "request exceeds 16MB limit")
			if err != nil {
				break
			}
			continue
		}
		if len(line) > 0 {
			s.handleLine(ctx, line)
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "agy-bridge: stdin error: %v\n", err)
				return err
			}
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil
}

func (s *mcpServer) waitInflight() {
	s.wg.Wait()
}
