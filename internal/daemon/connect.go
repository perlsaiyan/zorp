package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/ipc"
)

var ErrNotRunning = errors.New("daemon is not running")

const startTimeout = 5 * time.Second

// Connect dials the daemon. With autostart, a daemon that isn't running is
// started in the background (logging to the data dir) and waited for.
func Connect(ctx context.Context, cfg *config.Config, autostart bool) (*ipc.Client, error) {
	c, err := ipc.Dial(ctx, cfg.Paths.Socket)
	if err == nil {
		return c, nil
	}
	if !autostart {
		return nil, ErrNotRunning
	}
	if err := spawn(cfg); err != nil {
		return nil, fmt.Errorf("starting daemon: %w", err)
	}
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		if c, err := ipc.Dial(ctx, cfg.Paths.Socket); err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("daemon did not start within %s; see %s", startTimeout, cfg.Paths.DaemonLog)
}

func spawn(cfg *config.Config) error {
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(cfg.Paths.DaemonLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Own session, so the daemon outlives the terminal that started it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
