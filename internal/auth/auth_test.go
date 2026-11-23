package auth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExtractMeta(t *testing.T) {
	raw := `{"token":{"access_token":"ya29.fake","refresh_token":"1//ref-abc","token_type":"Bearer","expiry":"2026-11-23T14:54:24Z"},"id_token_email":"user@example.com"}`
	email, exp, fp := extractMeta(raw)
	if email != "user@example.com" {
		t.Fatalf("expected user@example.com, got %q", email)
	}
	if exp != "2026-11-23T14:54:24Z" {
		t.Fatalf("expected 2026-11-23T14:54:24Z, got %q", exp)
	}
	if len(fp) != 12 {
		t.Fatalf("expected 12-char refresh fingerprint, got %q", fp)
	}
}

func TestSaveListUseSwapAndSyncActive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGY_AUTH_DIR", tmpDir)

	origRead, origWrite := readCredFn, writeCredFn
	t.Cleanup(func() {
		readCredFn = origRead
		writeCredFn = origWrite
	})

	store := &RawCredential{
		Target:  TargetGeminiAntigravity,
		User:    "antigravity",
		Type:    1,
		Persist: 2,
		Blob:    []byte(`{"token":{"access_token":"tok-alt-1","refresh_token":"ref-alt","expiry":"2026-11-23T14:00:00Z"},"id_token_email":"alt@example.com"}`),
	}
	readCredFn = func(target string) (*RawCredential, error) {
		cp := *store
		cp.Blob = append([]byte(nil), store.Blob...)
		return &cp, nil
	}
	writeCredFn = func(raw *RawCredential) error {
		cp := *raw
		cp.Blob = append([]byte(nil), raw.Blob...)
		store = &cp
		return nil
	}

	if _, err := Save("../evil"); err == nil {
		t.Fatal("expected path traversal account name to fail")
	}

	altInfo, err := Save("alt")
	if err != nil {
		t.Fatalf("Save(alt) failed: %v", err)
	}
	if altInfo.Email != "alt@example.com" || altInfo.RefreshFP == "" {
		t.Fatalf("unexpected altInfo: %+v", altInfo)
	}

	// Simulate login to second account and save as main.
	store.Blob = []byte(`{"token":{"access_token":"tok-main-1","refresh_token":"ref-main","expiry":"2026-11-23T15:00:00Z"},"id_token_email":"main@example.com"}`)
	if _, err := Save("main"); err != nil {
		t.Fatalf("Save(main) failed: %v", err)
	}

	// Simulate access_token refresh on active main before switching away.
	store.Blob = []byte(`{"token":{"access_token":"tok-main-refreshed","refresh_token":"ref-main","expiry":"2026-11-23T16:00:00Z"},"id_token_email":"main@example.com"}`)

	list, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 2 || list[0].Name != "alt" || list[0].Active || list[1].Name != "main" || !list[1].Active {
		t.Fatalf("unexpected list state: %+v", list)
	}

	// Swap main -> alt (no reload).
	swapped, _, err := Swap(false, ReloadOptions{})
	if err != nil {
		t.Fatalf("Swap failed: %v", err)
	}
	if swapped.Name != "alt" || !strings.Contains(string(store.Blob), "tok-alt-1") {
		t.Fatalf("expected active store to switch to alt, got %+v / %s", swapped, string(store.Blob))
	}

	// Swap alt -> main and confirm the refreshed main token was synced before switching away earlier.
	swapped2, _, err := Swap(false, ReloadOptions{})
	if err != nil {
		t.Fatalf("second Swap failed: %v", err)
	}
	if swapped2.Name != "main" || !strings.Contains(string(store.Blob), "tok-main-refreshed") {
		t.Fatalf("expected synced tok-main-refreshed on swap back, got %s", string(store.Blob))
	}
}

func TestReloadAgyPanesUsesExactConversationIDAndSkipsBusy(t *testing.T) {
	t.Setenv("AGY_AUTH_DIR", t.TempDir())
	origHerdr, origSleep, origCompact := runHerdrFn, sleepFn, compactOverrideFn
	t.Cleanup(func() {
		runHerdrFn = origHerdr
		sleepFn = origSleep
		compactOverrideFn = origCompact
	})
	sleepFn = func(time.Duration) {}
	compactOverrideFn = func() string { return "200K" }

	listCalls := 0
	var sentCmds []string

	runHerdrFn = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
			listCalls++
			// Call 1: w15:p1 has agent_session; w14:p8 has no agent_session (will be read from /exit footer); w15:p3 is working.
			// Call 2: w15:p1 and w14:p8 exited.
			// Call 3 (second reload back-to-back): w15:p1 has empty agent_session -> must come from pane_sessions.json!
			// Call 4: w15:p1 exited.
			resp := herdrPaneListResp{}
			agentAfter := "agy"
			if listCalls == 2 || listCalls == 4 {
				agentAfter = ""
			}
			w15p1Session := "1e3beae7-c35d-4522-8b0c-5174649701e5"
			if listCalls >= 3 {
				w15p1Session = "" // simulate herdr not yet re-reporting agent_session before next message
			}
			resp.Result.Panes = []herdrPane{
				{PaneID: "w14:p1", Agent: "agy", AgentStatus: "working", CWD: `C:\Users\Anek`},
				{PaneID: "w15:p1", Agent: agentAfter, AgentStatus: "idle", CWD: `C:\Users\Anek\mandate`, AgentSession: struct {
					Agent string `json:"agent"`
					Kind  string `json:"kind"`
					Value string `json:"value"`
				}{Agent: "agy", Kind: "id", Value: w15p1Session}},
				{PaneID: "w15:p3", Agent: "agy", AgentStatus: "working", CWD: `C:\Users\Anek\assistant`, AgentSession: struct {
					Agent string `json:"agent"`
					Kind  string `json:"kind"`
					Value string `json:"value"`
				}{Agent: "agy", Kind: "id", Value: "cdbfe674-88d4-4a5c-8e0b-be64baaccb7e"}},
				{PaneID: "w14:p8", Agent: agentAfter, AgentStatus: "idle", CWD: `C:\Users\Anek`},
				{PaneID: "w15:p2", Agent: "omp", AgentStatus: "idle", CWD: `C:\Users\Anek\timbre`},
			}
			return json.Marshal(resp)
		}
		if len(args) >= 3 && args[0] == "pane" && args[1] == "read" {
			if args[2] == "w14:p8" && listCalls == 2 {
				return []byte("Resume with -c (or command below):\nagy --conversation=995ef162-24e3-416f-896e-5c2783c3d2c8\n"), nil
			}
			return []byte("C:\\Users\\Anek>\n"), nil
		}
		sentCmds = append(sentCmds, strings.Join(args, " "))
		return []byte("{}"), nil
	}

	res, err := ReloadAgyPanes(ReloadOptions{
		SkipPaneID:  "w14:p1",
		ForceBusy:   false,
		ExitTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("ReloadAgyPanes #1 failed: %v", err)
	}
	if len(res.SkippedBusy) != 1 || res.SkippedBusy[0] != "w15:p3" {
		t.Fatalf("expected w15:p3 skipped as busy, got %+v", res.SkippedBusy)
	}
	if len(res.Reloaded) != 2 {
		t.Fatalf("expected 2 reloaded panes, got %+v", res.Reloaded)
	}
	joined := strings.Join(sentCmds, "\n")
	if !strings.Contains(joined, "AGY_CLI_AUTO_COMPACT_OVERRIDE='200K'; agy --conversation=1e3beae7-c35d-4522-8b0c-5174649701e5") &&
		!strings.Contains(joined, "AGY_CLI_AUTO_COMPACT_OVERRIDE=200K agy --conversation=1e3beae7-c35d-4522-8b0c-5174649701e5") {
		t.Fatalf("expected auto-compact override + exact --conversation= resume for w15:p1, got:\n%s", joined)
	}
	if !strings.Contains(joined, "agy --conversation=995ef162-24e3-416f-896e-5c2783c3d2c8") {
		t.Fatalf("expected /exit footer parsed --conversation= for w14:p8, got:\n%s", joined)
	}
	if strings.Contains(joined, "w15:p3") || strings.Contains(joined, "w14:p1") || strings.Contains(joined, "w15:p2") {
		t.Fatalf("should not touch skipped/busy/omp panes, got:\n%s", joined)
	}

	// Second back-to-back reload when herdr reports empty agent_session and /exit prints no footer:
	// must recover both exact UUIDs from pane_sessions.json!
	sentCmds = nil
	res2, err := ReloadAgyPanes(ReloadOptions{
		SkipPaneID:  "w14:p1",
		ForceBusy:   false,
		ExitTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("ReloadAgyPanes #2 failed: %v", err)
	}
	if len(res2.Reloaded) != 2 {
		t.Fatalf("expected 2 reloaded panes on #2, got %+v", res2.Reloaded)
	}
	joined2 := strings.Join(sentCmds, "\n")
	if !strings.Contains(joined2, "agy --conversation=1e3beae7-c35d-4522-8b0c-5174649701e5") {
		t.Fatalf("expected cached --conversation= on back-to-back reload for w15:p1, got:\n%s", joined2)
	}
	if !strings.Contains(joined2, "agy --conversation=995ef162-24e3-416f-896e-5c2783c3d2c8") {
		t.Fatalf("expected cached --conversation= on back-to-back reload for w14:p8, got:\n%s", joined2)
	}

	// Reject malformed compact overrides (no shell injection).
	if got := buildRelaunchCommand("abc12345", "powershell.exe", "200K; calc.exe"); got != "agy --conversation=abc12345" {
		t.Fatalf("expected unsafe override to be ignored, got %q", got)
	}
}
