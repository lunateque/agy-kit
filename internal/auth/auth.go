package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/lunateque/agy-kit/internal/fsutil"
)

// Package-level seams for deterministic unit testing.
var (
	readCredFn  = readSysCredential
	writeCredFn = writeSysCredential
	runHerdrFn  = func(args ...string) ([]byte, error) {
		return exec.Command("herdr", args...).Output()
	}
	sleepFn          = time.Sleep
	compactOverrideFn = readAutoCompactOverride
)

func baseDir() string {
	if env := os.Getenv("AGY_AUTH_DIR"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agy-auth")
}

func ensureDir() (string, error) {
	d := baseDir()
	return d, os.MkdirAll(d, 0o700)
}

func extractMeta(rawJSON string) (email, expiry, refreshFP string) {
	var p oauthPayload
	if err := json.Unmarshal([]byte(rawJSON), &p); err == nil {
		email = strings.TrimSpace(p.IDTokenEmail)
		expiry = strings.TrimSpace(p.Token.Expiry)
		if rt := strings.TrimSpace(p.Token.RefreshToken); rt != "" {
			sum := sha256.Sum256([]byte(rt))
			refreshFP = hex.EncodeToString(sum[:6]) // 12 hex chars
		}
	}
	return email, expiry, refreshFP
}

func matchesActive(sc StoredCred, activeEmail, activeFP string) bool {
	if activeFP != "" {
		fp := sc.RefreshFP
		if fp == "" {
			_, _, fp = extractMeta(sc.RawJSON)
		}
		if fp != "" {
			return fp == activeFP
		}
	}
	if activeEmail != "" && sc.Email != "" {
		return strings.EqualFold(activeEmail, sc.Email)
	}
	return false
}

func Current() (*AccountInfo, error) {
	raw, err := readCredFn(TargetGeminiAntigravity)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", TargetGeminiAntigravity, err)
	}
	email, expiry, fp := extractMeta(string(raw.Blob))
	return &AccountInfo{
		Email:       email,
		RefreshFP:   fp,
		Active:      true,
		TokenExpiry: expiry,
	}, nil
}

func writeStoredCred(dir, name string, sc StoredCred) error {
	path := filepath.Join(dir, name+".json")
	if err := fsutil.SaveJSONIndentAtomic(path, sc); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

func Save(name string) (*AccountInfo, error) {
	name = strings.TrimSpace(name)
	if err := fsutil.ValidateStrictID(name); err != nil {
		return nil, fmt.Errorf("invalid account name %q: %w", name, err)
	}
	dir, err := ensureDir()
	if err != nil {
		return nil, err
	}

	raw, err := readCredFn(TargetGeminiAntigravity)
	if err != nil {
		return nil, fmt.Errorf("cannot read current %s from credential store: %w", TargetGeminiAntigravity, err)
	}

	email, expiry, fp := extractMeta(string(raw.Blob))
	sc := StoredCred{
		Name:        name,
		Email:       email,
		RefreshFP:   fp,
		Target:      raw.Target,
		User:        raw.User,
		Type:        raw.Type,
		Persist:     raw.Persist,
		RawJSON:     string(raw.Blob),
		SavedAt:     time.Now().UTC(),
		TokenExpiry: expiry,
	}

	if err := writeStoredCred(dir, name, sc); err != nil {
		return nil, fmt.Errorf("failed to save account file: %w", err)
	}

	return &AccountInfo{
		Name:        name,
		Email:       email,
		RefreshFP:   fp,
		Active:      true,
		TokenExpiry: expiry,
		SavedAt:     sc.SavedAt.Format(time.RFC3339),
	}, nil
}

// syncActiveProfile persists any refreshed access_token from the OS credential
// store back into its matching saved profile before switching away.
func syncActiveProfile() {
	raw, err := readCredFn(TargetGeminiAntigravity)
	if err != nil || len(raw.Blob) == 0 {
		return
	}
	activeEmail, activeExpiry, activeFP := extractMeta(string(raw.Blob))
	if activeEmail == "" && activeFP == "" {
		return
	}
	dir := baseDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if fsutil.ValidateStrictID(name) != nil {
			continue
		}
		data, err := fsutil.ReadFileShared(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var sc StoredCred
		if err := json.Unmarshal(data, &sc); err != nil {
			continue
		}
		if matchesActive(sc, activeEmail, activeFP) {
			if sc.RawJSON == string(raw.Blob) && sc.RefreshFP == activeFP && sc.Email == activeEmail {
				return
			}
			sc.Name = name
			sc.Email = activeEmail
			sc.RefreshFP = activeFP
			sc.RawJSON = string(raw.Blob)
			sc.TokenExpiry = activeExpiry
			sc.SavedAt = time.Now().UTC()
			_ = writeStoredCred(dir, name, sc)
			return
		}
	}
}

func List() ([]AccountInfo, error) {
	dir := baseDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var activeEmail, activeFP string
	if cur, err := Current(); err == nil {
		activeEmail = cur.Email
		activeFP = cur.RefreshFP
	}

	var list []AccountInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		p := filepath.Join(dir, e.Name())
		data, err := fsutil.ReadFileShared(p)
		if err != nil {
			continue
		}
		var sc StoredCred
		if err := json.Unmarshal(data, &sc); err != nil {
			continue
		}
		email, expiry, fp := sc.Email, sc.TokenExpiry, sc.RefreshFP
		if email == "" || expiry == "" || fp == "" {
			e2, exp2, fp2 := extractMeta(sc.RawJSON)
			if email == "" {
				email = e2
			}
			if expiry == "" {
				expiry = exp2
			}
			if fp == "" {
				fp = fp2
			}
		}
		sc.Email = email
		sc.RefreshFP = fp
		list = append(list, AccountInfo{
			Name:        name,
			Email:       email,
			RefreshFP:   fp,
			Active:      matchesActive(sc, activeEmail, activeFP),
			TokenExpiry: expiry,
			SavedAt:     sc.SavedAt.Format("2006-01-02 15:04"),
		})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})

	return list, nil
}

func Remove(name string) error {
	name = strings.TrimSpace(name)
	if err := fsutil.ValidateStrictID(name); err != nil {
		return fmt.Errorf("invalid account name %q: %w", name, err)
	}
	return os.Remove(filepath.Join(baseDir(), name+".json"))
}

func Use(name string, reload bool, opts ReloadOptions) (*AccountInfo, *ReloadResult, error) {
	name = strings.TrimSpace(name)
	if err := fsutil.ValidateStrictID(name); err != nil {
		return nil, nil, fmt.Errorf("invalid account name %q: %w", name, err)
	}
	dir := baseDir()
	path := filepath.Join(dir, name+".json")
	data, err := fsutil.ReadFileShared(path)
	if err != nil {
		return nil, nil, fmt.Errorf("account %q not found in %s: %w", name, dir, err)
	}

	var sc StoredCred
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, nil, fmt.Errorf("corrupt account file %s: %w", path, err)
	}

	// Preserve refreshed access token of whichever profile was active before switching.
	syncActiveProfile()

	raw := &RawCredential{
		Target:  sc.Target,
		User:    sc.User,
		Type:    sc.Type,
		Persist: sc.Persist,
		Blob:    []byte(sc.RawJSON),
	}
	if raw.Target == "" {
		raw.Target = TargetGeminiAntigravity
	}
	if raw.User == "" {
		raw.User = "antigravity"
	}

	if err := writeCredFn(raw); err != nil {
		return nil, nil, fmt.Errorf("failed to write %s to credential store: %w", raw.Target, err)
	}

	email, expiry, fp := extractMeta(sc.RawJSON)
	if email == "" {
		email = sc.Email
	}
	info := &AccountInfo{
		Name:        name,
		Email:       email,
		RefreshFP:   fp,
		Active:      true,
		TokenExpiry: expiry,
	}

	var rel *ReloadResult
	if reload {
		rel, err = ReloadAgyPanes(opts)
		if err != nil {
			return info, rel, err
		}
	}

	return info, rel, nil
}

func Swap(reload bool, opts ReloadOptions) (*AccountInfo, *ReloadResult, error) {
	accounts, err := List()
	if err != nil {
		return nil, nil, err
	}
	if len(accounts) < 2 {
		return nil, nil, fmt.Errorf("need at least 2 saved accounts to swap (found %d)", len(accounts))
	}

	activeIdx := -1
	for i, a := range accounts {
		if a.Active {
			activeIdx = i
			break
		}
	}
	nextIdx := 0
	if activeIdx >= 0 {
		nextIdx = (activeIdx + 1) % len(accounts)
	}

	return Use(accounts[nextIdx].Name, reload, opts)
}

type herdrPane struct {
	PaneID        string `json:"pane_id"`
	Agent         string `json:"agent"`
	AgentStatus   string `json:"agent_status"`
	CWD           string `json:"cwd"`
	TerminalTitle string `json:"terminal_title_stripped"`
	AgentSession  struct {
		Agent string `json:"agent"`
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"agent_session"`
}

type herdrPaneListResp struct {
	Result struct {
		Panes []herdrPane `json:"panes"`
	} `json:"result"`
}

func listHerdrPanes() ([]herdrPane, error) {
	out, err := runHerdrFn("pane", "list")
	if err != nil {
		return nil, fmt.Errorf("failed to query herdr: %w", err)
	}
	var resp herdrPaneListResp
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse herdr pane list: %w", err)
	}
	return resp.Result.Panes, nil
}

var (
	resumeConvRE      = regexp.MustCompile(`agy\s+--conversation(?:=|\s+)([A-Za-z0-9_-]{8,128})`)
	compactOverrideRE = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[kKmM%]?$`)
)

func buildRelaunchCommand(convID, termTitle, compactOverride string) string {
	base := "agy -c"
	if convID != "" {
		base = "agy --conversation=" + convID
	}
	v := strings.TrimSpace(compactOverride)
	if v == "" || !compactOverrideRE.MatchString(v) {
		return base
	}
	lowerTitle := strings.ToLower(termTitle)
	if runtime.GOOS == "windows" {
		if strings.Contains(lowerTitle, "cmd.exe") {
			return fmt.Sprintf("set AGY_CLI_AUTO_COMPACT_OVERRIDE=%s && %s", v, base)
		}
		return fmt.Sprintf("$env:AGY_CLI_AUTO_COMPACT_OVERRIDE='%s'; %s", v, base)
	}
	return fmt.Sprintf("AGY_CLI_AUTO_COMPACT_OVERRIDE=%s %s", v, base)
}

type paneSessionEntry struct {
	ConvID    string `json:"conv_id"`
	CWD       string `json:"cwd,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func paneSessionsPath() string {
	return filepath.Join(baseDir(), "pane_sessions.json")
}

func loadPaneSessions() map[string]paneSessionEntry {
	out := make(map[string]paneSessionEntry)
	data, err := fsutil.ReadFileShared(paneSessionsPath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

func savePaneSessions(m map[string]paneSessionEntry) {
	if _, err := ensureDir(); err != nil {
		return
	}
	_ = fsutil.SaveJSONIndentAtomic(paneSessionsPath(), m)
}

func normalizeCWD(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimRight(p, `/\`)
	return strings.ToLower(p)
}

// parseLastResumeConvID extracts the last "agy --conversation=<id>" printed by agy on /exit.
func parseLastResumeConvID(paneText string) string {
	matches := resumeConvRE.FindAllStringSubmatch(paneText, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		if len(matches[i]) >= 2 && fsutil.ValidateStrictID(matches[i][1]) == nil {
			return matches[i][1]
		}
	}
	return ""
}

func ReloadAgyPanes(opts ReloadOptions) (*ReloadResult, error) {
	panes, err := listHerdrPanes()
	if err != nil {
		return nil, err
	}

	timeout := opts.ExitTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	type targetPane struct {
		paneID    string
		cwd       string
		termTitle string
		convID    string
	}

	cached := loadPaneSessions()
	cacheDirty := false

	// Record any live agent_session IDs we observe across all agy panes (even skipped/busy ones).
	for _, p := range panes {
		if p.Agent == "agy" && p.AgentSession.Kind == "id" && fsutil.ValidateStrictID(p.AgentSession.Value) == nil {
			prev := cached[p.PaneID]
			if prev.ConvID != p.AgentSession.Value || normalizeCWD(prev.CWD) != normalizeCWD(p.CWD) {
				cached[p.PaneID] = paneSessionEntry{
					ConvID:    p.AgentSession.Value,
					CWD:       p.CWD,
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				}
				cacheDirty = true
			}
		}
	}

	res := &ReloadResult{}
	var targets []targetPane

	for _, p := range panes {
		if p.Agent != "agy" {
			continue
		}
		if opts.SkipPaneID != "" && p.PaneID == opts.SkipPaneID {
			continue
		}
		if p.AgentStatus == "working" && !opts.ForceBusy {
			res.SkippedBusy = append(res.SkippedBusy, p.PaneID)
			continue
		}
		convID := ""
		if p.AgentSession.Kind == "id" && fsutil.ValidateStrictID(p.AgentSession.Value) == nil {
			convID = p.AgentSession.Value
		} else if prev, ok := cached[p.PaneID]; ok && fsutil.ValidateStrictID(prev.ConvID) == nil {
			if prev.CWD == "" || p.CWD == "" || normalizeCWD(prev.CWD) == normalizeCWD(p.CWD) {
				convID = prev.ConvID
			}
		}
		targets = append(targets, targetPane{paneID: p.PaneID, cwd: p.CWD, termTitle: p.TerminalTitle, convID: convID})
	}

	if len(targets) == 0 {
		if cacheDirty {
			savePaneSessions(cached)
		}
		return res, nil
	}

	// Step 1: Ask each target agy session to exit cleanly via /exit.
	pending := make(map[string]targetPane, len(targets))
	for _, t := range targets {
		if _, err := runHerdrFn("pane", "send-text", t.paneID, "/exit"); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", t.paneID, err))
			continue
		}
		if _, err := runHerdrFn("pane", "send-keys", t.paneID, "Enter"); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", t.paneID, err))
			continue
		}
		pending[t.paneID] = t
	}

	// Step 2: Poll herdr until each pane returns to shell (agent != "agy").
	// If still running after half the timeout, send double Ctrl+C fallback.
	deadline := time.Now().Add(timeout)
	fallbackAt := time.Now().Add(timeout / 2)
	sentFallback := false
	exited := make([]targetPane, 0, len(pending))

	for len(pending) > 0 {
		sleepFn(250 * time.Millisecond)
		curPanes, err := listHerdrPanes()
		if err == nil {
			byID := make(map[string]herdrPane, len(curPanes))
			for _, cp := range curPanes {
				byID[cp.PaneID] = cp
			}
			for id, t := range pending {
				cp, ok := byID[id]
				if !ok || cp.Agent != "agy" {
					exited = append(exited, t)
					delete(pending, id)
				}
			}
		}
		if len(pending) == 0 {
			break
		}
		now := time.Now()
		if !sentFallback && now.After(fallbackAt) {
			sentFallback = true
			for id := range pending {
				_, _ = runHerdrFn("pane", "send-keys", id, "C-c")
				sleepFn(150 * time.Millisecond)
				_, _ = runHerdrFn("pane", "send-keys", id, "C-c")
			}
		}
		if now.After(deadline) {
			for id := range pending {
				res.Failed = append(res.Failed, fmt.Sprintf("%s: timed out waiting for agy to exit", id))
			}
			break
		}
	}

	// Step 3: Read the /exit footer printed by agy ("agy --conversation=<id>"),
	// persist it in pane_sessions.json, and relaunch the exact same conversation.
	compactVal := compactOverrideFn()
	for _, t := range exited {
		if out, err := runHerdrFn("pane", "read", t.paneID, "--lines", "25"); err == nil {
			if parsed := parseLastResumeConvID(string(out)); parsed != "" {
				t.convID = parsed
			}
		}
		if t.convID != "" {
			cached[t.paneID] = paneSessionEntry{
				ConvID:    t.convID,
				CWD:       t.cwd,
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			cacheDirty = true
		}

		cmd := buildRelaunchCommand(t.convID, t.termTitle, compactVal)
		if _, err := runHerdrFn("pane", "send-text", t.paneID, cmd); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", t.paneID, err))
			continue
		}
		if _, err := runHerdrFn("pane", "send-keys", t.paneID, "Enter"); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", t.paneID, err))
			continue
		}
		res.Reloaded = append(res.Reloaded, t.paneID)
	}

	if cacheDirty {
		savePaneSessions(cached)
	}

	return res, nil
}

func parseReloadFlags(args []string) (reload bool, opts ReloadOptions) {
	reload = true
	opts = ReloadOptions{
		SkipPaneID: os.Getenv("HERDR_PANE_ID"),
	}
	for _, arg := range args {
		switch {
		case arg == "--no-reload":
			reload = false
		case arg == "--reload":
			reload = true
		case arg == "--force":
			opts.ForceBusy = true
		case arg == "--all":
			opts.SkipPaneID = ""
		case strings.HasPrefix(arg, "--skip="):
			opts.SkipPaneID = strings.TrimPrefix(arg, "--skip=")
		}
	}
	return reload, opts
}

func printReloadSummary(rel *ReloadResult) {
	if rel == nil {
		return
	}
	if len(rel.Reloaded) > 0 {
		fmt.Printf("Reloaded %d agy pane(s): %s\n", len(rel.Reloaded), strings.Join(rel.Reloaded, ", "))
	} else {
		fmt.Println("No idle agy panes needed reload.")
	}
	if len(rel.SkippedBusy) > 0 {
		fmt.Printf("Skipped busy (working) pane(s): %s (pass --force to interrupt)\n", strings.Join(rel.SkippedBusy, ", "))
	}
	if len(rel.Failed) > 0 {
		fmt.Fprintf(os.Stderr, "Failed panes: %s\n", strings.Join(rel.Failed, "; "))
	}
}

func Run(args []string) int {
	if len(args) < 1 {
		printUsage()
		return 1
	}

	switch args[0] {
	case "save":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: agy-kit auth save <name>")
			return 1
		}
		info, err := Save(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Printf("Saved account %q (%s, fp=%s)\n", info.Name, info.Email, info.RefreshFP)

	case "list", "ls":
		accounts, err := List()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if len(accounts) == 0 {
			fmt.Println("No accounts saved yet in", baseDir())
			fmt.Println("Run 'agy-kit auth save <name>' to save current active account.")
			return 0
		}
		fmt.Printf("%-12s %-30s %-14s %-10s %s\n", "NAME", "EMAIL", "REFRESH_FP", "STATUS", "TOKEN EXPIRY")
		for _, a := range accounts {
			status := "stored"
			if a.Active {
				status = "* active"
			}
			exp := a.TokenExpiry
			if len(exp) >= 19 {
				exp = strings.ReplaceAll(exp[:19], "T", " ")
			}
			fmt.Printf("%-12s %-30s %-14s %-10s %s\n", a.Name, a.Email, a.RefreshFP, status, exp)
		}

	case "current", "whoami":
		info, err := Current()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Printf("Active: %s (fp=%s, expires=%s)\n", info.Email, info.RefreshFP, info.TokenExpiry)

	case "use":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: agy-kit auth use <name> [--no-reload] [--force] [--skip=<pane>] [--all]")
			return 1
		}
		reload, opts := parseReloadFlags(args[2:])
		info, rel, err := Use(args[1], reload, opts)
		if info != nil {
			fmt.Printf("Switched to account %q (%s)\n", info.Name, info.Email)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if reload {
			printReloadSummary(rel)
		}

	case "swap", "next":
		reload, opts := parseReloadFlags(args[1:])
		info, rel, err := Swap(reload, opts)
		if info != nil {
			fmt.Printf("Swapped to account %q (%s)\n", info.Name, info.Email)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if reload {
			printReloadSummary(rel)
		}

	case "reload":
		_, opts := parseReloadFlags(args[1:])
		rel, err := ReloadAgyPanes(opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		printReloadSummary(rel)

	case "menu", "ui", "pick":
		return runMenu(args[1:])

	case "rm", "remove", "delete":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: agy-kit auth rm <name>")
			return 1
		}
		if err := Remove(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Printf("Removed saved account %q\n", args[1])

	default:
		fmt.Fprintln(os.Stderr, "unknown auth command:", args[0])
		printUsage()
		return 1
	}

	return 0
}

func printUsage() {
	fmt.Println(`usage: agy-kit auth <command>

Commands:
  save <name>                                    Save active agy OAuth credential under <name>
  list                                           List saved accounts and highlight active one
  current                                        Show active account in Windows Credential Manager
  use <name> [--no-reload] [--force] [--skip=ID] Switch account and relaunch idle agy panes in herdr
  swap [--no-reload] [--force] [--skip=ID]       Cycle to the next saved account and relaunch agy panes
  reload [--force] [--skip=ID] [--all]           Relaunch agy panes in herdr preserving exact --conversation IDs
  menu                                           Interactive popup picker (bind in herdr [[keys.command]])
  rm <name>                                      Delete a saved account profile`)
}
