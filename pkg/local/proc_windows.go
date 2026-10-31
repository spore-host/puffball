//go:build windows

package local

import "os/exec"

// ownProcessGroup is a no-op on Windows; cancel kills uv, and WaitDelay
// bounds the wait for its children's pipes.
func ownProcessGroup(*exec.Cmd) {}
