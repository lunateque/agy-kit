# Watchdog Review Priorities

Especially watch for:
- Silent error swallowing, race conditions, or missing cleanup on cancellation/timeout.
- Windows vs Unix portability traps (file locks, rename sharing violations, console flags, path separators).
- Claims of "done" or "passing" without running the actual build/test/verification command.
- Any credential, token, or private file accidentally printed or committed.
