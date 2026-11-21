package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lunateque/agy-kit/internal/fsutil"
	"github.com/lunateque/agy-kit/internal/proc"
)

type HistoryRecord struct {
	Timestamp  string  `json:"ts"`
	Epoch      int     `json:"epoch"`
	FromStep   int     `json:"from_step"`
	ToStep     int     `json:"to_step"`
	Severity   string  `json:"severity"` // "silent" | "nit" | "concern" | "blocker" | "error"
	Key        string  `json:"key,omitempty"`
	Note       string  `json:"note,omitempty"`
	Error      string  `json:"error,omitempty"`
	CacheRead  int64   `json:"cache_read"`
	InputTok   int64   `json:"in"`
	OutputTok  int64   `json:"out"`
	CostUSD    float64 `json:"cost_usd"`
	DurationMs int64   `json:"dur_ms"`
}

// SpawnDetachedWorker starts a background `agy-kit advisor worker --conv <id>`
// process that survives the short-lived hook process exit.
func SpawnDetachedWorker(convID string) error {
	safe := fsutil.SafeID(convID)
	if err := fsutil.ValidateStrictID(safe); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	env := append(os.Environ(), "AGY_ADVISOR_CHILD=1")
	usedFallback, err := proc.StartDetached(exe, []string{"advisor", "worker", "--conv", safe}, "", env)
	if err == nil && usedFallback {
		AppendHistory(GetConvPaths(safe), HistoryRecord{
			Timestamp: time.Now().Format(time.RFC3339),
			Severity:  "info",
			Note:      "breakaway-from-job denied; spawned detached worker with CREATE_NO_WINDOW fallback",
		})
	}
	return err
}

// RunWorker executes the single-flight background drain loop for one conversation.
// Uses an OS kernel lock (LockFileEx / Flock) and a post-release pending re-check
// to eliminate stale locks, lost wake-ups, and silent lock-error swallowing.
func RunWorker(ctx context.Context, convID string, cfg Config, runner RunnerFunc) error {
	if err := fsutil.ValidateStrictID(convID); err != nil {
		return err
	}
	if runner == nil {
		runner = ExecuteAdvisor
	}
	paths := GetConvPaths(convID)
	if err := paths.EnsureDirs(); err != nil {
		return err
	}

	for {
		lk, err := fsutil.TryLockFile(paths.WorkerLock)
		if err != nil {
			if errors.Is(err, fsutil.ErrLocked) {
				// Another live worker already owns this conversation.
				return nil
			}
			// Surface genuine OS/permission errors rather than silently going dark!
			return fmt.Errorf("worker lock failed: %w", err)
		}

		shouldRecheck := drainPendingLoop(ctx, convID, paths, cfg, runner)
		lk.Unlock()

		// Lost wake-up prevention: always re-check pending/ after Unlock unless
		// we entered backoff/quota or hit context cancellation.
		if !shouldRecheck || len(ListPendingFiles(paths)) == 0 {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func drainPendingLoop(ctx context.Context, convID string, paths ConvPaths, cfg Config, runner RunnerFunc) bool {
	meta, err := LoadMeta(paths.Meta)
	if err != nil {
		// Never overwrite live meta on transient read/parse errors.
		return false
	}
	meta.ConversationID = convID
	meta.Backend = cfg.Backend
	meta.Model = cfg.Model

	// Recover any leftover inflight files from an earlier crashed worker.
	recoverInflight(paths)

	for {
		if ctx.Err() != nil {
			return false
		}
		nowMs := time.Now().UnixMilli()
		if meta.NextRetryAtMs > nowMs {
			_ = SaveMetaAndBadge(paths, meta, cfg.AdvisorName)
			return false
		}

		cur, cerr := LoadCursor(paths.Cursor)
		if cerr != nil {
			return false
		}
		batches := claimPendingBatches(paths, cur.SessionEpoch)
		if len(batches) == 0 {
			meta.Status = "idle"
			_ = SaveMetaAndBadge(paths, meta, cfg.AdvisorName)
			return true
		}

		coalesced := coalesceBatches(batches, cfg.MaxDeltaBytes)
		scopeDir := ResolveScopeDir(coalesced.CWD)
		if scopeDir == "" {
			// Skip review when CWD is empty or $HOME (never expose ~/.ssh or ~/.aws).
			cleanupInflight(paths)
			if coalesced.ToStep > meta.ReviewedThroughStep {
				meta.ReviewedThroughStep = coalesced.ToStep
			}
			meta.Status = "idle"
			_ = SaveMetaAndBadge(paths, meta, cfg.AdvisorName)
			continue
		}

		// Rotate advisor session if epoch changed OR if scope directory changed
		// (Claude stores sessions per project dir, so --resume breaks across dirs).
		if meta.SessionEpoch != cur.SessionEpoch || (meta.ScopeDir != "" && !strings.EqualFold(filepath.Clean(meta.ScopeDir), filepath.Clean(scopeDir))) {
			meta.SessionEpoch = cur.SessionEpoch
			meta.SessionRotation = 0
			meta.ScopeDir = scopeDir
			meta.HasSession = false
			meta.AdvisorSessionID = ""
			meta.LastCacheRead = 0
			meta.ReviewedThroughStep = -1
			meta.SeenKeys = map[string]string{}
		}
		if meta.ScopeDir == "" {
			meta.ScopeDir = scopeDir
		}

		// Auto-rotate advisor session once cached context exceeds max_cache_tokens
		// so long coding sessions don't balloon Anthropic cache_read costs.
		// Keeps SeenKeys intact so previously raised issues are not re-emitted.
		promptText := coalesced.DeltaText
		if meta.HasSession && cfg.MaxCacheTokens > 0 && meta.LastCacheRead >= cfg.MaxCacheTokens {
			prevCache := meta.LastCacheRead
			meta.SessionRotation++
			meta.HasSession = false
			meta.AdvisorSessionID = ""
			meta.LastCacheRead = 0
			AppendHistory(paths, HistoryRecord{
				Timestamp: time.Now().Format(time.RFC3339),
				Epoch:     meta.SessionEpoch,
				FromStep:  coalesced.FromStep,
				ToStep:    coalesced.ToStep,
				Severity:  "info",
				Key:       "auto-rotate",
				Note:      fmt.Sprintf("rotated advisor session r%d (cache %d >= %d)", meta.SessionRotation, prevCache, cfg.MaxCacheTokens),
			})
			promptText = fmt.Sprintf("[Advisor session auto-rotated to keep context lean; resuming review from step %d]\n\n%s", coalesced.FromStep, promptText)
		}

		epochSessionDir := filepath.Join(paths.Sessions, fmt.Sprintf("epoch-%d-r%d-%s", meta.SessionEpoch, meta.SessionRotation, shortPathHash(scopeDir)))
		_ = os.MkdirAll(epochSessionDir, 0o755)
		sysPromptPath := filepath.Join(epochSessionDir, "system.md")
		if !meta.HasSession {
			watchdogs := DiscoverWatchdogFiles(coalesced.CWD, cfg.AllowProjectWatchdog)
			promptContent := BuildSystemPrompt(watchdogs)
			_ = fsutil.WriteBytesAtomic(sysPromptPath, []byte(promptContent))
		}

		meta.Status = "reviewing"
		_ = SaveMetaAndBadge(paths, meta, cfg.AdvisorName)

		res, runErr := runner(ctx, RunRequest{
			Backend:          cfg.Backend,
			Model:            cfg.Model,
			Thinking:         cfg.Thinking,
			ScopeDir:         scopeDir,
			SessionDir:       epochSessionDir,
			SystemPromptPath: sysPromptPath,
			HasSession:       meta.HasSession,
			AdvisorSessionID: meta.AdvisorSessionID,
			Prompt:           promptText,
			TimeoutSec:       cfg.CallTimeoutSec,
		})

		if runErr != nil {
			meta.ConsecutiveFailures++
			meta.LastError = trimCapUTF8(runErr.Error(), 500)
			if res != nil && res.QuotaExceeded {
				cleanupInflight(paths)
				meta.Status = "quota"
				meta.NextRetryAtMs = time.Now().Add(15 * time.Minute).UnixMilli()
			} else if meta.ConsecutiveFailures >= 3 {
				// Bound poison batches: drop inflight once circuit breaker trips.
				cleanupInflight(paths)
				meta.Status = "backoff"
				meta.NextRetryAtMs = time.Now().Add(backoffDuration(meta.ConsecutiveFailures)).UnixMilli()
			} else {
				// Transient failure (<3): preserve the delta back into pending/ so
				// no transcript gap is created before the next attempt!
				recoverInflight(paths)
				meta.Status = "idle"
			}
			AppendHistory(paths, HistoryRecord{
				Timestamp: time.Now().Format(time.RFC3339),
				Epoch:     meta.SessionEpoch,
				FromStep:  coalesced.FromStep,
				ToStep:    coalesced.ToStep,
				Severity:  "error",
				Error:     meta.LastError,
			})
			_ = SaveMetaAndBadge(paths, meta, cfg.AdvisorName)
			return false
		}

		// Review succeeded — remove inflight files now.
		cleanupInflight(paths)

		meta.ConsecutiveFailures = 0
		meta.NextRetryAtMs = 0
		meta.LastError = ""
		meta.ReviewsCount++
		meta.HasSession = true
		if res.SessionID != "" {
			meta.AdvisorSessionID = res.SessionID
		}
		meta.LastCacheRead = res.CacheReadTokens
		meta.LastInputTokens = res.InputTokens
		meta.LastOutputTokens = res.OutputTokens
		meta.TotalCostUSD += res.CostUSD

		// Epoch fence: if primary compacted/reset while Opus was thinking, drop stale notes.
		curAfter, caErr := LoadCursor(paths.Cursor)
		if caErr != nil || curAfter.SessionEpoch != meta.SessionEpoch {
			meta.Status = "idle"
			_ = SaveMetaAndBadge(paths, meta, cfg.AdvisorName)
			continue
		}

		parsed := ParseAdvisorResponse(res.Response)
		histSev := "silent"

		if !parsed.Silent {
			prevSev := meta.SeenKeys[parsed.Key]
			if prevSev != "" && SeverityRank(parsed.Severity) <= SeverityRank(prevSev) {
				parsed.Silent = true
			} else {
				// For WIP non-blockers, write to ready/ with Deferred=true instead of
				// discarding (Opus already believes it sent this note in its session).
				deferred := coalesced.WIP && parsed.Severity != "blocker"
				note := ReadyNote{
					V:            StateSchemaVersion,
					ID:           fmt.Sprintf("e%d-s%d-%d", meta.SessionEpoch, coalesced.ToStep, time.Now().UnixNano()%100000),
					SessionEpoch: meta.SessionEpoch,
					AdvisorName:  cfg.AdvisorName,
					Severity:     parsed.Severity,
					Key:          parsed.Key,
					Note:         parsed.Note,
					Deferred:     deferred,
					FromStep:     coalesced.FromStep,
					ToStep:       coalesced.ToStep,
					CreatedMs:    time.Now().UnixMilli(),
				}
				// Record SeenKeys ONLY after WriteReadyNote succeeds!
				if werr := WriteReadyNote(paths, note); werr == nil {
					meta.SeenKeys[parsed.Key] = parsed.Severity
					meta.NotesCount++
					if parsed.Severity == "blocker" {
						meta.BlockersCount++
					}
					histSev = parsed.Severity
				}
			}
		}

		// Commit watermark ONLY AFTER ready/ note is written.
		if coalesced.ToStep > meta.ReviewedThroughStep {
			meta.ReviewedThroughStep = coalesced.ToStep
		}
		meta.Status = "idle"
		AppendHistory(paths, HistoryRecord{
			Timestamp:  time.Now().Format(time.RFC3339),
			Epoch:      meta.SessionEpoch,
			FromStep:   coalesced.FromStep,
			ToStep:     coalesced.ToStep,
			Severity:   histSev,
			Key:        parsed.Key,
			Note:       parsed.Note,
			CacheRead:  res.CacheReadTokens,
			InputTok:   res.InputTokens,
			OutputTok:  res.OutputTokens,
			CostUSD:    res.CostUSD,
			DurationMs: res.DurationMs,
		})
		_ = SaveMetaAndBadge(paths, meta, cfg.AdvisorName)
	}
}

func shortPathHash(p string) string {
	var h uint64 = 14695981039346656037
	for _, b := range []byte(strings.ToLower(filepath.Clean(p))) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return fmt.Sprintf("%08x", uint32(h))
}

func backoffDuration(failures int) time.Duration {
	switch {
	case failures <= 3:
		return 2 * time.Minute
	case failures == 4:
		return 5 * time.Minute
	default:
		return 15 * time.Minute
	}
}

func recoverInflight(paths ConvPaths) {
	entries, err := os.ReadDir(paths.Inflight)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		_ = os.Rename(filepath.Join(paths.Inflight, e.Name()), filepath.Join(paths.Pending, e.Name()))
	}
}

func cleanupInflight(paths ConvPaths) {
	entries, err := os.ReadDir(paths.Inflight)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			_ = os.Remove(filepath.Join(paths.Inflight, e.Name()))
		}
	}
}

func claimPendingBatches(paths ConvPaths, activeEpoch int) []PendingBatch {
	names := ListPendingFiles(paths)
	if len(names) == 0 {
		return nil
	}
	var batches []PendingBatch
	for _, name := range names {
		src := filepath.Join(paths.Pending, name)
		dst := filepath.Join(paths.Inflight, name)
		if err := os.Rename(src, dst); err != nil {
			continue
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			_ = os.Remove(dst)
			continue
		}
		var b PendingBatch
		if json.Unmarshal(data, &b) != nil {
			_ = os.Remove(dst)
			continue
		}
		if activeEpoch > 0 && b.SessionEpoch != activeEpoch {
			_ = os.Remove(dst)
			continue
		}
		batches = append(batches, b)
	}
	return batches
}

func coalesceBatches(batches []PendingBatch, maxBytes int) PendingBatch {
	if len(batches) == 1 {
		return batches[0]
	}
	merged := batches[0]
	var kept []PendingBatch
	used := 0
	for i := len(batches) - 1; i >= 0; i-- {
		b := batches[i]
		// Strip trailing WIP footer from non-final coalesced chunks.
		body := strings.TrimSuffix(b.DeltaText, wipFooter)
		if len(kept) > 0 && maxBytes > 0 && used+len(body)+len(wipFooter) > maxBytes {
			break
		}
		b.DeltaText = body
		kept = append(kept, b)
		used += len(body)
	}
	// Reverse kept back to chronological order.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	var sb strings.Builder
	for i, b := range kept {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(b.DeltaText)
	}
	last := batches[len(batches)-1]
	merged.FromStep = kept[0].FromStep
	merged.ToStep = last.ToStep
	merged.WIP = last.WIP
	if last.CWD != "" {
		merged.CWD = last.CWD
	}
	if merged.WIP {
		sb.WriteString(wipFooter)
	}
	merged.DeltaText = sb.String()
	return merged
}
