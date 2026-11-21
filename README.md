# agy-kit

Zero-dependency Go toolkit for **Antigravity CLI (`agy`)** combining three subsystems into one fast static binary:

1. **`telemetry`** — Per-call & session TPS, cache hit %, quota buckets, width-adaptive Tokyo Night statusline (with live advisor badge), and historical `stats`.
2. **`bridge`** — One-way MCP stdio server (`agy → omp / claude`) for delegating heavy reviews or secondary tasks to external harnesses without risking Google consumer OAuth ToS bans.
3. **`advisor`** — `oh-my-pi`-style **shadow Opus advisor** wired into `agy` `hooks.json`:
   - Uses your **Claude subscription** (via `omp` `auth-broker` or `claude` CLI) — **zero Google/agy `3p` quota burned**.
   - Maintains a **persistent per-conversation session** with incremental byte-offset deltas (`~99%` Anthropic ephemeral prompt cache hit on follow-up turns).
   - Non-blocking background worker (`LockFileEx` / `flock` single-flight + lock-free spool dirs) so normal turns never stall.
   - Injects `<advisory>` XML + inline `user_markdown` cards on `PreInvocation`/`PostInvocation`, and enforces a bounded `Stop` gate for `blocker` findings.
   - Reads `WATCHDOG.md` from user (`~/.agy-advisor/WATCHDOG.md`, `~/.omp/agent/WATCHDOG.md`) and project (`WATCHDOG.md`, `.omp/WATCHDOG.md`, `.agents/WATCHDOG.md`).

## Build

```bash
go build -o bin/agy-kit ./cmd/agy-kit
# Windows:
go build -o bin/agy-kit.exe ./cmd/agy-kit
```

## Quick Commands

```bash
agy-kit advisor doctor              # Verify backend binary, auth, scope & WATCHDOG.md
agy-kit advisor status              # Active conversations, cache hits, cost, last error
agy-kit advisor enable | disable    # Toggle shadow advisor globally
agy-kit advisor reset               # Rotate sessions and clear backoff circuit breakers
agy-kit advisor dump                # Show latest advisor review log
agy-kit telemetry stats today       # Per-model TPS & cache stats
agy-kit telemetry config show       # Telemetry display configuration
```

See `examples/` for `hooks.json`, `mcp_config.json`, and `WATCHDOG.md` templates.
