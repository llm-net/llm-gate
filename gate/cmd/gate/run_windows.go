//go:build windows

package main

import "os/exec"

// Console Ctrl+C is delivered by Windows to gate and the attached child in the
// same console group. Keeping that group intact also preserves interactive TTY
// behavior for Codex, Grok and Claude Code.
func runTool(cmd *exec.Cmd) error { return cmd.Run() }
