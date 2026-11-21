package advisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/lunateque/agy-kit/internal/fsutil"
)

// Config controls the agy-advisor shadow reviewer subsystem.
type Config struct {
	Enabled              bool   `json:"enabled"`
	Backend              string `json:"backend"`                // "omp" (default, Claude sub via auth-broker) | "claude"
	Model                string `json:"model"`                  // e.g. "anthropic/claude-opus-5-5" or "opus"
	Thinking             string `json:"thinking"`               // "low" | "medium" | "high"
	AdvisorName          string `json:"advisor_name"`           // display label in <advisory> and statusline, e.g. "Opus"
	StopWaitSec          int    `json:"stop_wait_sec"`          // bounded Stop hook catch-up wait (default 10s)
	CallTimeoutSec       int    `json:"call_timeout_sec"`       // hard per-invocation worker timeout (default 120s)
	ImmuneTurns          int    `json:"immune_turns"`           // cooldown user turns after concern injection (blockers exempt!)
	MaxDeltaBytes        int    `json:"max_delta_bytes"`        // cap formatted delta size per batch (default 48KB)
	ShowCardInChat       bool   `json:"show_card_in_chat"`      // render metadata.user_markdown advisory cards in agy UI
	AllowProjectWatchdog bool   `json:"allow_project_watchdog"` // opt-in for repo-level WATCHDOG.md (default false for prompt-injection safety)
}

func DefaultConfig() Config {
	return Config{
		Enabled:              true,
		Backend:              "omp",
		Model:                "anthropic/claude-opus-5-5",
		Thinking:             "medium",
		AdvisorName:          "Opus",
		StopWaitSec:          10,
		CallTimeoutSec:       120,
		ImmuneTurns:          3,
		MaxDeltaBytes:        48 * 1024,
		ShowCardInChat:       true,
		AllowProjectWatchdog: false,
	}
}

// BaseDir returns ~/.agy-advisor (overridable via AGY_ADVISOR_DIR for tests).
func BaseDir() string {
	if env := strings.TrimSpace(os.Getenv("AGY_ADVISOR_DIR")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agy-advisor")
}

func ConfigPath() string {
	return filepath.Join(BaseDir(), "config.json")
}

func LoadConfig() Config {
	cfg := DefaultConfig()
	data, err := fsutil.ReadFileShared(ConfigPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	cfg.Backend = strings.ToLower(strings.TrimSpace(cfg.Backend))
	if cfg.Backend != "omp" && cfg.Backend != "claude" {
		cfg.Backend = "omp"
	}
	if strings.TrimSpace(cfg.Model) == "" {
		if cfg.Backend == "claude" {
			cfg.Model = "opus"
		} else {
			cfg.Model = "anthropic/claude-opus-5-5"
		}
	}
	if strings.TrimSpace(cfg.Thinking) == "" {
		cfg.Thinking = "medium"
	}
	if strings.TrimSpace(cfg.AdvisorName) == "" {
		cfg.AdvisorName = "Opus"
	}
	if cfg.StopWaitSec < 0 {
		cfg.StopWaitSec = 0
	} else if cfg.StopWaitSec > 30 {
		cfg.StopWaitSec = 30
	}
	if cfg.CallTimeoutSec < 15 {
		cfg.CallTimeoutSec = 15
	} else if cfg.CallTimeoutSec > 300 {
		cfg.CallTimeoutSec = 300
	}
	if cfg.ImmuneTurns < 0 {
		cfg.ImmuneTurns = 0
	}
	if cfg.MaxDeltaBytes < 4096 {
		cfg.MaxDeltaBytes = 48 * 1024
	}
	return cfg
}

func SaveConfig(cfg Config) error {
	return fsutil.SaveJSONIndentAtomic(ConfigPath(), cfg)
}

// FindGitRoot walks up from startDir looking for a .git directory or file.
func FindGitRoot(startDir string) string {
	if startDir == "" {
		return ""
	}
	cur, err := filepath.Abs(startDir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// ResolveScopeDir returns the repo root if cwd is inside a git repo, or cwd itself.
// Returns "" if cwd is empty or resolves to the user's home directory, so the
// advisor never runs with home-wide read access (~/.ssh, ~/.aws, etc.).
func ResolveScopeDir(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	home, herr := os.UserHomeDir()
	absHome := ""
	if herr == nil {
		absHome, _ = filepath.Abs(home)
	}
	scope := absCWD
	if root := FindGitRoot(absCWD); root != "" {
		scope = root
	}
	if absHome != "" && strings.EqualFold(filepath.Clean(scope), filepath.Clean(absHome)) {
		// Never scope an advisor to the entire home directory.
		return ""
	}
	return scope
}

// isAncestorPath reports whether anc is equal to or an ancestor directory of target.
func isAncestorPath(anc, target string) bool {
	if anc == "" || target == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(anc), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// WatchdogFile represents one discovered WATCHDOG.md guidance file.
type WatchdogFile struct {
	Path    string
	Content string
}

// DiscoverWatchdogFiles loads user-level WATCHDOG.md (~/.agy-advisor/WATCHDOG.md
// and ~/.omp/agent/WATCHDOG.md). Project-level WATCHDOG.md files are ONLY loaded
// when allowProject is true, and the walk is strictly bounded to ancestors inside
// the git root (never walking to /tmp or filesystem root).
func DiscoverWatchdogFiles(cwd string, allowProject bool) []WatchdogFile {
	seen := map[string]bool{}
	var out []WatchdogFile
	addFile := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		key := strings.ToLower(filepath.Clean(abs))
		if seen[key] {
			return
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return
		}
		trimmed := strings.TrimSpace(string(data))
		if trimmed == "" {
			return
		}
		seen[key] = true
		out = append(out, WatchdogFile{Path: abs, Content: trimmed})
	}

	// 1. User-level watchdogs (always trusted)
	addFile(filepath.Join(BaseDir(), "WATCHDOG.md"))
	if home, err := os.UserHomeDir(); err == nil {
		addFile(filepath.Join(home, ".omp", "agent", "WATCHDOG.md"))
	}

	// 2. Project-level watchdogs (opt-in only; prevents untrusted repo prompt injection)
	if !allowProject || strings.TrimSpace(cwd) == "" {
		return out
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return out
	}
	stopDir := FindGitRoot(absCWD)
	if stopDir == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			absHome, _ := filepath.Abs(home)
			if isAncestorPath(absHome, absCWD) {
				stopDir = absHome
			}
		}
	}
	// If stopDir is not an ancestor of cwd (e.g. /tmp/x or D:\proj without .git),
	// stop at cwd itself so we never load /tmp/WATCHDOG.md or drive-root files.
	if stopDir == "" || !isAncestorPath(stopDir, absCWD) {
		stopDir = absCWD
	}

	var chain []string
	cur := absCWD
	for {
		chain = append(chain, cur)
		if strings.EqualFold(filepath.Clean(cur), filepath.Clean(stopDir)) {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	// Reverse so ancestor comes first, cwd leaf comes last.
	for i := len(chain) - 1; i >= 0; i-- {
		dir := chain[i]
		addFile(filepath.Join(dir, "WATCHDOG.md"))
		addFile(filepath.Join(dir, ".omp", "WATCHDOG.md"))
		addFile(filepath.Join(dir, ".agents", "WATCHDOG.md"))
	}
	return out
}
