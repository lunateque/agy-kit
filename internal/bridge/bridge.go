package bridge

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func Run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mgr := newJobManager()
	srv := newMCPServer(mgr, os.Stdout)

	_ = srv.serve(ctx, os.Stdin)

	// Cancel root context and wait briefly for tracked child process trees to terminate cleanly.
	cancel()
	mgr.cancelAllAndWait(3 * time.Second)
	srv.waitInflight()
	return 0
}
