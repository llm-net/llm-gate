//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func runTool(cmd *exec.Cmd) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case sig := <-signals:
			// The child shares the terminal process group and normally receives
			// interactive signals directly. This explicit delivery also covers a
			// signal sent only to the gate process (systemd, kill, or a supervisor).
			_ = cmd.Process.Signal(sig)
		}
	}
}
