package advisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lunateque/agy-kit/internal/fsutil"
)

const StateSchemaVersion = 1

// ConvPaths groups all lock-free spool directories and files for one conversation.
type ConvPaths struct {
	Root       string
	Cursor     string
	Meta       string
	Badge      string
	WorkerLock string
	Pending    string
	Inflight   string
	Ready      string
	Delivered  string
	Sessions   string
	History    string
}

func GetConvPaths(convID string) ConvPaths {
	safe := fsutil.SafeID(convID)
	root := filepath.Join(BaseDir(), "convs", safe)
	return ConvPaths{
		Root:       root,
		Cursor:     filepath.Join(root, "cursor.json"),
		Meta:       filepath.Join(root, "meta.json"),
		Badge:      filepath.Join(root, "badge.json"),
		WorkerLock: filepath.Join(root, "worker.lock"),
		Pending:    filepath.Join(root, "pending"),
		Inflight:   filepath.Join(root, "inflight"),
		Ready:      filepath.Join(root, "ready"),
		Delivered:  filepath.Join(root, "delivered"),
		Sessions:   filepath.Join(root, "sessions"),
		History:    filepath.Join(root, "history.jsonl"),
	}
}

func (p ConvPaths) EnsureDirs() error {
	for _, d := range []string{p.Root, p.Pending, p.Inflight, p.Ready, p.Delivered, p.Sessions} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// CursorState is owned exclusively by hooks (serialized via cursor.lock).
type CursorState struct {
	V                int    `json:"v"`
	ConversationID   string `json:"conversation_id"`
	TranscriptPath   string `json:"transcript_path"`
	CWD              string `json:"cwd"`
	ByteOffset       int64  `json:"byte_offset"`
	FileSize         int64  `json:"file_size"`
	FirstLineHash    string `json:"first_line_hash"`
	LastStepIndex    int    `json:"last_step_index"`
	SessionEpoch     int    `json:"session_epoch"`
	NextSeq          int64  `json:"next_seq"`
	TurnsSinceAdvice int    `json:"turns_since_advice"`
	TurnHasWork      bool   `json:"turn_has_work"`
	UpdatedMs        int64  `json:"updated_ms"`
}

// PendingBatch is one spooled transcript delta written by hooks and claimed by worker.
type PendingBatch struct {
	V            int      `json:"v"`
	Seq          int64    `json:"seq"`
	SessionEpoch int      `json:"session_epoch"`
	FromStep     int      `json:"from_step"`
	ToStep       int      `json:"to_step"`
	WIP          bool     `json:"wip"`
	CWD          string   `json:"cwd"`
	DeltaText    string   `json:"delta_text"`
	TouchedFiles []string `json:"touched_files,omitempty"`
	CreatedMs    int64    `json:"created_ms"`
}

// ReadyNote is one validated advice note written by worker and claimed by hooks.
type ReadyNote struct {
	V            int    `json:"v"`
	ID           string `json:"id"`
	SessionEpoch int    `json:"session_epoch"`
	AdvisorName  string `json:"advisor_name"`
	Severity     string `json:"severity"` // "nit" | "concern" | "blocker"
	Key          string `json:"key"`      // stable dedupe key e.g. "file:symbol:issue"
	Note         string `json:"note"`
	Deferred     bool   `json:"deferred,omitempty"` // true for WIP nit/concern held until next turn PreInvocation
	FromStep     int    `json:"from_step"`
	ToStep       int    `json:"to_step"`
	CreatedMs    int64  `json:"created_ms"`
}

// WorkerMeta is owned exclusively by the detached worker.
type WorkerMeta struct {
	V                   int               `json:"v"`
	ConversationID      string            `json:"conversation_id"`
	Status              string            `json:"status"` // "idle" | "reviewing" | "backoff" | "quota" | "disabled"
	Backend             string            `json:"backend"`
	Model               string            `json:"model"`
	ScopeDir            string            `json:"scope_dir,omitempty"`
	SessionEpoch        int               `json:"session_epoch"`
	SessionRotation     int               `json:"session_rotation,omitempty"`
	AdvisorSessionID    string            `json:"advisor_session_id,omitempty"`
	HasSession          bool              `json:"has_session"`
	ReviewedThroughStep int               `json:"reviewed_through_step"`
	ReviewsCount        int               `json:"reviews_count"`
	NotesCount          int               `json:"notes_count"`
	BlockersCount       int               `json:"blockers_count"`
	ConsecutiveFailures int               `json:"consecutive_failures"`
	NextRetryAtMs       int64             `json:"next_retry_at_ms,omitempty"`
	LastError           string            `json:"last_error,omitempty"`
	LastCacheRead       int64             `json:"last_cache_read"`
	LastInputTokens     int64             `json:"last_input_tokens"`
	LastOutputTokens    int64             `json:"last_output_tokens"`
	TotalCostUSD        float64           `json:"total_cost_usd"`
	SeenKeys            map[string]string `json:"seen_keys,omitempty"`
	UpdatedMs           int64             `json:"updated_ms"`
}

// BadgeState is the minimal v:1 status file read by agy-telemetry's statusline.
type BadgeState struct {
	V          int    `json:"v"`
	Status     string `json:"status"`
	Advisor    string `json:"advisor"`
	NotesCount int    `json:"notes_count"`
	Pending    int    `json:"pending"`
	UpdatedMs  int64  `json:"updated_ms"`
}

func defaultCursor() CursorState {
	return CursorState{
		V:                StateSchemaVersion,
		LastStepIndex:    -1,
		SessionEpoch:     1,
		TurnsSinceAdvice: 999,
	}
}

func defaultMeta() WorkerMeta {
	return WorkerMeta{
		V:                   StateSchemaVersion,
		Status:              "idle",
		ReviewedThroughStep: -1,
		SeenKeys:            map[string]string{},
	}
}

// LoadCursor reads cursor.json. Returns defaults ONLY when the file does not exist;
// surfaces transient read/parse/version errors so callers never overwrite live state.
func LoadCursor(path string) (CursorState, error) {
	data, err := fsutil.ReadFileShared(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return defaultCursor(), nil
		}
		return CursorState{}, err
	}
	var parsed CursorState
	if err := json.Unmarshal(data, &parsed); err != nil {
		return CursorState{}, err
	}
	if parsed.V != StateSchemaVersion {
		return CursorState{}, fmt.Errorf("unsupported cursor schema version %d", parsed.V)
	}
	if parsed.SessionEpoch < 1 {
		parsed.SessionEpoch = 1
	}
	return parsed, nil
}

func SaveCursor(path string, st CursorState) error {
	st.V = StateSchemaVersion
	st.UpdatedMs = time.Now().UnixMilli()
	return fsutil.SaveJSONAtomic(path, st)
}

// LoadMeta reads meta.json. Returns defaults ONLY when the file does not exist;
// surfaces transient read/parse/version errors so the worker never resets session state.
func LoadMeta(path string) (WorkerMeta, error) {
	data, err := fsutil.ReadFileShared(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return defaultMeta(), nil
		}
		return WorkerMeta{}, err
	}
	var parsed WorkerMeta
	if err := json.Unmarshal(data, &parsed); err != nil {
		return WorkerMeta{}, err
	}
	if parsed.V != StateSchemaVersion {
		return WorkerMeta{}, fmt.Errorf("unsupported meta schema version %d", parsed.V)
	}
	if parsed.SeenKeys == nil {
		parsed.SeenKeys = map[string]string{}
	}
	return parsed, nil
}

func SaveMetaAndBadge(paths ConvPaths, m WorkerMeta, advisorName string) error {
	m.V = StateSchemaVersion
	m.UpdatedMs = time.Now().UnixMilli()
	if err := fsutil.SaveJSONAtomic(paths.Meta, m); err != nil {
		return err
	}
	pendingCount := countJSONFiles(paths.Pending) + countJSONFiles(paths.Inflight)
	badge := BadgeState{
		V:          StateSchemaVersion,
		Status:     m.Status,
		Advisor:    advisorName,
		NotesCount: m.NotesCount,
		Pending:    pendingCount,
		UpdatedMs:  m.UpdatedMs,
	}
	return fsutil.SaveJSONAtomic(paths.Badge, badge)
}

func countJSONFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			n++
		}
	}
	return n
}

// HasPendingOrInflight reports whether any batch is waiting or actively running.
func HasPendingOrInflight(paths ConvPaths) bool {
	return countJSONFiles(paths.Pending) > 0 || countJSONFiles(paths.Inflight) > 0
}

// EnqueuePending atomically writes a delta batch into pending/.
func EnqueuePending(paths ConvPaths, batch PendingBatch) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	batch.V = StateSchemaVersion
	batch.CreatedMs = time.Now().UnixMilli()
	name := fmt.Sprintf("%012d-e%d.json", batch.Seq, batch.SessionEpoch)
	return fsutil.SaveJSONAtomic(filepath.Join(paths.Pending, name), batch)
}

// WriteReadyNote atomically writes a validated advice note into ready/.
func WriteReadyNote(paths ConvPaths, note ReadyNote) error {
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	note.V = StateSchemaVersion
	if note.CreatedMs == 0 {
		note.CreatedMs = time.Now().UnixMilli()
	}
	name := fmt.Sprintf("%016d-%s.json", note.CreatedMs, fsutil.SafeID(note.ID))
	return fsutil.SaveJSONAtomic(filepath.Join(paths.Ready, name), note)
}

// ClaimReadyNotes atomically moves matching notes from ready/ -> delivered/.
//   - blockersOnly: when true (cooldown or Stop gate), non-blockers stay untouched in ready/.
//   - allowDeferred: when false (PostInvocation mid-turn), deferred WIP notes stay in ready/
//     until the next turn's PreInvocation.
func ClaimReadyNotes(paths ConvPaths, currentEpoch int, blockersOnly, allowDeferred bool) []ReadyNote {
	entries, err := os.ReadDir(paths.Ready)
	if err != nil || len(entries) == 0 {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var claimed []ReadyNote
	for _, name := range names {
		src := filepath.Join(paths.Ready, name)
		data, err := fsutil.ReadFileShared(src)
		if err != nil {
			continue
		}
		var note ReadyNote
		if json.Unmarshal(data, &note) != nil || note.V != StateSchemaVersion {
			_ = os.Remove(src)
			continue
		}
		if currentEpoch > 0 && note.SessionEpoch != currentEpoch {
			_ = os.Remove(src)
			continue
		}
		if blockersOnly && note.Severity != "blocker" {
			continue
		}
		if !allowDeferred && note.Deferred && note.Severity != "blocker" {
			continue
		}
		dst := filepath.Join(paths.Delivered, name)
		if err := os.Rename(src, dst); err != nil {
			continue
		}
		claimed = append(claimed, note)
	}
	return claimed
}

// ListPendingFiles returns sorted filenames inside pending/.
func ListPendingFiles(paths ConvPaths) []string {
	entries, err := os.ReadDir(paths.Pending)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// AppendHistory appends one advisor review record to history.jsonl.
func AppendHistory(paths ConvPaths, rec any) {
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(paths.History, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}
