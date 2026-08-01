//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
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

// copyOwner gives dst the same owner as src, so rewriting a file gora runs
// as an unprivileged user does not leave it owned by root.
func copyOwner(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return nil // nothing to copy from
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(dst, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("keeping the owner of %s: %w", src, err)
	}
	return nil
}
