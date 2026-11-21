package advisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lunateque/agy-kit/internal/fsutil"
)

type hookPayload struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
	ModelName             string   `json:"modelName"`
	CWD                   string   `json:"cwd"`
	InvocationNum         int      `json:"invocationNum"`
	InitialNumSteps       int      `json:"initialNumSteps"`
	ExecutionNum          int      `json:"executionNum"`
	TerminationReason     string   `json:"terminationReason"`
	ContinuationActive    bool     `json:"continuationActive"`
}

type injectedStep struct {
	SystemMessage *struct {
		SystemMessage string         `json:"systemMessage"`
		Metadata      map[string]any `json:"metadata,omitempty"`
	} `json:"systemMessage,omitempty"`
}

type invocationHookOutput struct {
	InjectSteps []injectedStep `json:"injectSteps,omitempty"`
}

type stopHookOutput struct {
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Run is the CLI entrypoint for `agy-kit advisor <subcommand>`.
func Run(args []string) int {
	if len(args) < 1 {
		usage()
		return 1
	}
	switch args[0] {
	case "hook":
		if len(args) < 2 {
			fmt.Print("{}")
			return 0
		}
		return runHook(args[1])
	case "worker":
		return runWorkerCLI(args[1:])
	case "status":
		return runStatus(args[1:])
	case "enable":
		return setEnabled(true)
	case "disable":
		return setEnabled(false)
	case "reset":
		return runReset(args[1:])
	case "dump":
		return runDump(args[1:])
	case "doctor":
		return runDoctor()
	case "config":
		return runConfigCLI(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown advisor command:", args[0])
		usage()
		return 1
	}
}

func usage() {
	fmt.Println("usage: agy-kit advisor <hook pre-invocation|post-invocation|stop | worker --conv <id> | status | enable | disable | reset | dump | doctor | config [show|init|set <k> <v>]>")
}

func runHook(event string) int {
	if os.Getenv("AGY_ADVISOR_CHILD") == "1" {
		fmt.Print("{}")
		return 0
	}

	cfg := LoadConfig()
	if !cfg.Enabled {
		fmt.Print("{}")
		return 0
	}

	raw, _ := io.ReadAll(os.Stdin)
	var in hookPayload
	if len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &in)
	}
	if in.ConversationID == "" {
		fmt.Print("{}")
		return 0
	}

	cwd := in.CWD
	if cwd == "" && len(in.WorkspacePaths) > 0 {
		cwd = in.WorkspacePaths[0]
	}

	switch event {
	case "pre-invocation":
		writeJSON(handlePreInvocation(in.ConversationID, cfg))
	case "post-invocation":
		writeJSON(handlePostInvocation(in.ConversationID, in.TranscriptPath, cwd, cfg))
	case "stop":
		writeJSON(handleStop(in, cwd, cfg))
	default:
		fmt.Print("{}")
	}
	return 0
}

func writeJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		fmt.Print("{}")
		return
	}
	_, _ = os.Stdout.Write(data)
}

func handlePreInvocation(convID string, cfg Config) invocationHookOutput {
	paths := GetConvPaths(convID)
	cur, err := LoadCursor(paths.Cursor)
	if err != nil {
		return invocationHookOutput{}
	}
	// Check cooldown BEFORE claiming so cooled-down nit/concern notes remain safely in ready/.
	inCooldown := cfg.ImmuneTurns > 0 && cur.TurnsSinceAdvice < cfg.ImmuneTurns
	notes := ClaimReadyNotes(paths, cur.SessionEpoch, inCooldown, true)
	if len(notes) == 0 {
		return invocationHookOutput{}
	}
	return buildInjectOutput(paths, notes, cfg)
}

func handlePostInvocation(convID, transcriptPath, cwd string, cfg Config) invocationHookOutput {
	paths := GetConvPaths(convID)
	_ = paths.EnsureDirs()

	var cur CursorState
	var shouldSpawn bool
	cursorLock := filepath.Join(paths.Root, "cursor.lock")
	locked := fsutil.WithKernelLock(cursorLock, 400*time.Millisecond, func() {
		loaded, err := LoadCursor(paths.Cursor)
		if err != nil {
			return
		}
		cur = loaded
		cur.ConversationID = convID
		if transcriptPath != "" {
			cur.TranscriptPath = transcriptPath
		}
		if cwd != "" {
			cur.CWD = cwd
		}

		delta := ReadTranscriptDelta(cur.TranscriptPath, cur, cfg.MaxDeltaBytes, true)
		if delta.ResetEpoch {
			cur.SessionEpoch++
		}
		if delta.HasWork {
			cur.TurnHasWork = true
		}
		cur.ByteOffset = delta.NewOffset
		cur.FileSize = delta.FileSize
		cur.FirstLineHash = delta.FirstHash
		if delta.ToStep > cur.LastStepIndex || delta.ResetEpoch {
			cur.LastStepIndex = delta.ToStep
		}

		// Enqueue only coding work in a valid non-home workspace scope.
		if delta.HasDelta && (delta.HasWork || cur.TurnHasWork) && ResolveScopeDir(cur.CWD) != "" {
			cur.NextSeq++
			_ = EnqueuePending(paths, PendingBatch{
				Seq:          cur.NextSeq,
				SessionEpoch: cur.SessionEpoch,
				FromStep:     delta.FromStep,
				ToStep:       delta.ToStep,
				WIP:          true,
				CWD:          cur.CWD,
				DeltaText:    delta.Text,
				TouchedFiles: delta.TouchedFiles,
			})
			shouldSpawn = true
		}
		_ = SaveCursor(paths.Cursor, cur)
	})
	if !locked || cur.V == 0 {
		return invocationHookOutput{}
	}

	if shouldSpawn {
		_ = SpawnDetachedWorker(convID)
	}

	inCooldown := cfg.ImmuneTurns > 0 && cur.TurnsSinceAdvice < cfg.ImmuneTurns
	// Mid-turn: don't claim Deferred WIP notes; those wait for next turn's PreInvocation.
	notes := ClaimReadyNotes(paths, cur.SessionEpoch, inCooldown, false)
	if len(notes) == 0 {
		return invocationHookOutput{}
	}
	return buildInjectOutput(paths, notes, cfg)
}

func handleStop(in hookPayload, cwd string, cfg Config) stopHookOutput {
	paths := GetConvPaths(in.ConversationID)
	_ = paths.EnsureDirs()
	cursorLock := filepath.Join(paths.Root, "cursor.lock")

	// Early exits (error/cancel/continuationActive): still reset per-turn work flag
	// and advance user-turn counter so state never bleeds into the next turn!
	if (in.TerminationReason != "" && in.TerminationReason != "NO_TOOL_CALL") || in.ContinuationActive {
		_ = fsutil.WithKernelLock(cursorLock, 200*time.Millisecond, func() {
			if cur, err := LoadCursor(paths.Cursor); err == nil {
				cur.TurnHasWork = false
				if !in.ContinuationActive {
					cur.TurnsSinceAdvice++
				}
				_ = SaveCursor(paths.Cursor, cur)
			}
		})
		return stopHookOutput{}
	}

	var targetStep int
	var epoch int
	var hadWork bool
	var enqueued bool
	locked := fsutil.WithKernelLock(cursorLock, 400*time.Millisecond, func() {
		cur, err := LoadCursor(paths.Cursor)
		if err != nil {
			return
		}
		cur.ConversationID = in.ConversationID
		if in.TranscriptPath != "" {
			cur.TranscriptPath = in.TranscriptPath
		}
		if cwd != "" {
			cur.CWD = cwd
		}

		delta := ReadTranscriptDelta(cur.TranscriptPath, cur, cfg.MaxDeltaBytes, false)
		if delta.ResetEpoch {
			cur.SessionEpoch++
		}
		if delta.HasWork {
			cur.TurnHasWork = true
		}
		hadWork = cur.TurnHasWork
		cur.ByteOffset = delta.NewOffset
		cur.FileSize = delta.FileSize
		cur.FirstLineHash = delta.FirstHash
		if delta.ToStep > cur.LastStepIndex || delta.ResetEpoch {
			cur.LastStepIndex = delta.ToStep
		}
		targetStep = cur.LastStepIndex
		epoch = cur.SessionEpoch

		if delta.HasDelta && hadWork && ResolveScopeDir(cur.CWD) != "" {
			cur.NextSeq++
			_ = EnqueuePending(paths, PendingBatch{
				Seq:          cur.NextSeq,
				SessionEpoch: cur.SessionEpoch,
				FromStep:     delta.FromStep,
				ToStep:       delta.ToStep,
				WIP:          false,
				CWD:          cur.CWD,
				DeltaText:    delta.Text,
				TouchedFiles: delta.TouchedFiles,
			})
			enqueued = true
		}
		// Count completed user turns here (not per-tool invocation) and reset per-turn work.
		cur.TurnsSinceAdvice++
		cur.TurnHasWork = false
		_ = SaveCursor(paths.Cursor, cur)
	})
	if !locked || !hadWork {
		return stopHookOutput{}
	}
	if enqueued {
		_ = SpawnDetachedWorker(in.ConversationID)
	}

	// Bounded catch-up wait: exits immediately when watermark catches up, when
	// worker is idle with empty spool (no wasted 10s wait!), or on backoff/quota.
	if cfg.StopWaitSec > 0 && targetStep >= 0 {
		deadline := time.Now().Add(time.Duration(cfg.StopWaitSec) * time.Second)
		for time.Now().Before(deadline) {
			if blockers := ClaimReadyNotes(paths, epoch, true, true); len(blockers) > 0 {
				return stopHookOutput{
					Decision: "continue",
					Reason:   joinAdvisoryMessages(blockers),
				}
			}
			meta, merr := LoadMeta(paths.Meta)
			if merr == nil {
				if meta.Status == "backoff" || meta.Status == "quota" {
					break
				}
				if meta.SessionEpoch == epoch && meta.ReviewedThroughStep >= targetStep {
					break
				}
				if meta.Status != "reviewing" && !HasPendingOrInflight(paths) {
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	if blockers := ClaimReadyNotes(paths, epoch, true, true); len(blockers) > 0 {
		return stopHookOutput{
			Decision: "continue",
			Reason:   joinAdvisoryMessages(blockers),
		}
	}
	return stopHookOutput{}
}

func joinAdvisoryMessages(notes []ReadyNote) string {
	var parts []string
	for _, n := range notes {
		parts = append(parts, FormatAdvisorySystemMessage(n))
	}
	return strings.Join(parts, "\n\n")
}

func buildInjectOutput(paths ConvPaths, notes []ReadyNote, cfg Config) invocationHookOutput {
	sort.SliceStable(notes, func(i, j int) bool {
		return SeverityRank(notes[i].Severity) > SeverityRank(notes[j].Severity)
	})

	var steps []injectedStep
	triggeredCooldown := false

	for _, n := range notes {
		var meta map[string]any
		if cfg.ShowCardInChat {
			meta = map[string]any{
				"user_markdown": FormatAdvisoryUserMarkdown(n),
			}
		}
		steps = append(steps, injectedStep{
			SystemMessage: &struct {
				SystemMessage string         `json:"systemMessage"`
				Metadata      map[string]any `json:"metadata,omitempty"`
			}{
				SystemMessage: FormatAdvisorySystemMessage(n),
				Metadata:      meta,
			},
		})
		if n.Severity == "concern" {
			triggeredCooldown = true
		}
	}

	if triggeredCooldown {
		cursorLock := filepath.Join(paths.Root, "cursor.lock")
		_ = fsutil.WithKernelLock(cursorLock, 200*time.Millisecond, func() {
			if c, err := LoadCursor(paths.Cursor); err == nil {
				c.TurnsSinceAdvice = 0
				_ = SaveCursor(paths.Cursor, c)
			}
		})
	}
	return invocationHookOutput{InjectSteps: steps}
}

func runWorkerCLI(args []string) int {
	var convID string
	for i := 0; i < len(args); i++ {
		if args[i] == "--conv" && i+1 < len(args) {
			convID = args[i+1]
			i++
		}
	}
	if err := fsutil.ValidateStrictID(convID); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 1
	}
	cfg := LoadConfig()
	if !cfg.Enabled {
		return 0
	}
	if err := RunWorker(context.Background(), convID, cfg, ExecuteAdvisor); err != nil {
		fmt.Fprintln(os.Stderr, "worker error:", err)
		return 1
	}
	return 0
}

func setEnabled(enabled bool) int {
	cfg := LoadConfig()
	cfg.Enabled = enabled
	if err := SaveConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "failed to save config:", err)
		return 1
	}
	if enabled {
		fmt.Printf("Advisor ENABLED (%s / %s, thinking=%s)\n", cfg.Backend, cfg.Model, cfg.Thinking)
	} else {
		fmt.Println("Advisor DISABLED")
	}
	return 0
}

func runStatus(args []string) int {
	cfg := LoadConfig()
	fmt.Printf("Advisor: enabled=%v backend=%s model=%s thinking=%s stop_wait=%ds project_watchdog=%v\n",
		cfg.Enabled, cfg.Backend, cfg.Model, cfg.Thinking, cfg.StopWaitSec, cfg.AllowProjectWatchdog)

	convsDir := filepath.Join(BaseDir(), "convs")
	entries, err := os.ReadDir(convsDir)
	if err != nil || len(entries) == 0 {
		fmt.Println("No advisor sessions recorded yet.")
		return 0
	}

	type row struct {
		id   string
		meta WorkerMeta
	}
	var rows []row
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := GetConvPaths(e.Name())
		m, merr := LoadMeta(p.Meta)
		if merr != nil {
			continue
		}
		rows = append(rows, row{id: e.Name(), meta: m})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].meta.UpdatedMs > rows[j].meta.UpdatedMs
	})
	if len(rows) > 8 {
		rows = rows[:8]
	}

	fmt.Printf("\n%-14s %-10s %5s %5s %7s %8s %7s %s\n", "CONV", "STATUS", "REVS", "NOTES", "STEP", "CACHE", "COST", "LAST_ERROR")
	for _, r := range rows {
		cid := r.id
		if len(cid) > 12 {
			cid = cid[:12] + ".."
		}
		errTxt := trimCapUTF8(r.meta.LastError, 45)
		fmt.Printf("%-14s %-10s %5d %5d %7d %8d $%6.3f %s\n",
			cid, r.meta.Status, r.meta.ReviewsCount, r.meta.NotesCount,
			r.meta.ReviewedThroughStep, r.meta.LastCacheRead, r.meta.TotalCostUSD, errTxt)
	}
	return 0
}

func runReset(args []string) int {
	convsDir := filepath.Join(BaseDir(), "convs")
	entries, err := os.ReadDir(convsDir)
	if err != nil {
		fmt.Println("Nothing to reset.")
		return 0
	}
	cfg := LoadConfig()
	resetCount, busyCount := 0, 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := GetConvPaths(e.Name())
		// Acquire worker.lock before mutating meta so a running worker cannot overwrite our reset.
		wlk, werr := fsutil.TryLockFile(p.WorkerLock)
		if werr != nil {
			if errors.Is(werr, fsutil.ErrLocked) {
				busyCount++
				continue
			}
			continue
		}
		cursorLock := filepath.Join(p.Root, "cursor.lock")
		_ = fsutil.WithKernelLock(cursorLock, 500*time.Millisecond, func() {
			if cur, err := LoadCursor(p.Cursor); err == nil {
				cur.SessionEpoch++
				_ = SaveCursor(p.Cursor, cur)
			}
		})
		if meta, err := LoadMeta(p.Meta); err == nil {
			meta.Status = "idle"
			meta.ConsecutiveFailures = 0
			meta.NextRetryAtMs = 0
			meta.LastError = ""
			meta.HasSession = false
			meta.AdvisorSessionID = ""
			meta.SeenKeys = map[string]string{}
			_ = SaveMetaAndBadge(p, meta, cfg.AdvisorName)
		}
		wlk.Unlock()
		resetCount++
	}
	fmt.Printf("Reset %d advisor conversation(s) (%d busy/skipped).\n", resetCount, busyCount)
	return 0
}

func runDump(args []string) int {
	convsDir := filepath.Join(BaseDir(), "convs")
	entries, err := os.ReadDir(convsDir)
	if err != nil || len(entries) == 0 {
		fmt.Println("No advisor history found.")
		return 0
	}
	var latestPath string
	var latestMod time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		hp := filepath.Join(convsDir, e.Name(), "history.jsonl")
		if info, serr := os.Stat(hp); serr == nil && info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
			latestPath = hp
		}
	}
	if latestPath == "" {
		fmt.Println("No advisor history entries yet.")
		return 0
	}
	f, err := os.Open(latestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	fmt.Println("=== Advisor History:", latestPath, "===")
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var h HistoryRecord
		if json.Unmarshal(sc.Bytes(), &h) == nil {
			fmt.Printf("[%s] e%d step %d-%d %-7s cache=%d $%.4f %s %s\n",
				h.Timestamp, h.Epoch, h.FromStep, h.ToStep, strings.ToUpper(h.Severity),
				h.CacheRead, h.CostUSD, h.Key, firstLine(h.Note+h.Error))
		}
	}
	return 0
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return trimCapUTF8(s, 100)
}

func runDoctor() int {
	cfg := LoadConfig()
	fmt.Printf("Config:   %s (enabled=%v)\n", ConfigPath(), cfg.Enabled)
	fmt.Printf("Backend:  %s | model=%s | thinking=%s\n", cfg.Backend, cfg.Model, cfg.Thinking)

	binPath, err := CheckBackendAvailable(cfg.Backend)
	if err != nil {
		fmt.Printf("Binary:   FAIL (%v)\n", err)
		return 1
	}
	fmt.Printf("Binary:   OK (%s)\n", binPath)

	cwd, _ := os.Getwd()
	scope := ResolveScopeDir(cwd)
	if scope == "" {
		fmt.Printf("Scope:    SKIP (cwd=%s is home or empty; advisor activates inside project dirs)\n", cwd)
	} else {
		fmt.Printf("Scope:    %s\n", scope)
	}

	wds := DiscoverWatchdogFiles(cwd, cfg.AllowProjectWatchdog)
	fmt.Printf("Watchdog: %d file(s) (allow_project_watchdog=%v)\n", len(wds), cfg.AllowProjectWatchdog)
	for _, w := range wds {
		fmt.Printf("          - %s\n", w.Path)
	}

	tmpDir, err := os.MkdirTemp("", "agy-advisor-doctor-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "doctor temp dir failed:", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)
	sysPath := filepath.Join(tmpDir, "system.md")
	_ = os.WriteFile(sysPath, []byte("Reply with exactly: SILENT"), 0o644)

	probeScope := scope
	if probeScope == "" {
		probeScope = tmpDir
	}

	fmt.Println("Probing backend live (1 minimal turn)...")
	res, err := ExecuteAdvisor(context.Background(), RunRequest{
		Backend:          cfg.Backend,
		Model:            cfg.Model,
		Thinking:         "low",
		ScopeDir:         probeScope,
		SessionDir:       filepath.Join(tmpDir, "sess"),
		SystemPromptPath: sysPath,
		Prompt:           "### Session update\nUser said ping. Assistant said pong.",
		TimeoutSec:       45,
	})
	if err != nil {
		fmt.Printf("Probe:    FAIL (%v)\n", err)
		return 1
	}
	fmt.Printf("Probe:    OK (model=%s provider=%s resp=%q dur=%dms cache_read=%d cost=$%.4f)\n",
		res.Model, res.Provider, res.Response, res.DurationMs, res.CacheReadTokens, res.CostUSD)
	return 0
}

func runConfigCLI(args []string) int {
	if len(args) == 0 || args[0] == "show" {
		cfg := LoadConfig()
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Printf("# %s\n%s\n", ConfigPath(), string(data))
		return 0
	}
	switch args[0] {
	case "init":
		if err := SaveConfig(DefaultConfig()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("Wrote default advisor config to", ConfigPath())
		return 0
	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: agy-kit advisor config set <key> <value>")
			return 1
		}
		cfg := LoadConfig()
		k, v := args[1], args[2]
		switch k {
		case "enabled":
			b, err := strconv.ParseBool(v)
			if err != nil {
				fmt.Fprintln(os.Stderr, "enabled must be true/false")
				return 1
			}
			cfg.Enabled = b
		case "backend":
			if v != "omp" && v != "claude" {
				fmt.Fprintln(os.Stderr, "backend must be omp or claude")
				return 1
			}
			cfg.Backend = v
		case "model":
			if err := fsutil.ValidateSafeArg(v); err != nil {
				fmt.Fprintln(os.Stderr, "invalid model:", err)
				return 1
			}
			cfg.Model = v
		case "thinking":
			if err := fsutil.ValidateSafeArg(v); err != nil {
				fmt.Fprintln(os.Stderr, "invalid thinking:", err)
				return 1
			}
			cfg.Thinking = v
		case "advisor_name":
			cfg.AdvisorName = v
		case "stop_wait_sec":
			n, err := strconv.Atoi(v)
			if err != nil {
				fmt.Fprintln(os.Stderr, "stop_wait_sec must be integer")
				return 1
			}
			cfg.StopWaitSec = n
		case "immune_turns":
			n, err := strconv.Atoi(v)
			if err != nil {
				fmt.Fprintln(os.Stderr, "immune_turns must be integer")
				return 1
			}
			cfg.ImmuneTurns = n
		case "show_card_in_chat":
			b, err := strconv.ParseBool(v)
			if err != nil {
				fmt.Fprintln(os.Stderr, "show_card_in_chat must be true/false")
				return 1
			}
			cfg.ShowCardInChat = b
		case "allow_project_watchdog":
			b, err := strconv.ParseBool(v)
			if err != nil {
				fmt.Fprintln(os.Stderr, "allow_project_watchdog must be true/false")
				return 1
			}
			cfg.AllowProjectWatchdog = b
		default:
			fmt.Fprintln(os.Stderr, "unknown key:", k)
			return 1
		}
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("Updated %s = %s in %s\n", k, v, ConfigPath())
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: agy-kit advisor config [show | init | set <k> <v>]")
		return 1
	}
}
