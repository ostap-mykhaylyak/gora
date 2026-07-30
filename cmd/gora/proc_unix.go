//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether the pid names a live process. EPERM means the
// process exists and belongs to somebody else — still alive, so a non-root
// `gora status` does not conclude that the service is down.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func hangup(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }
