//go:build windows

package main

import (
	"errors"
	"os"
)

// gora runs on Linux; these stubs exist so the tree builds and the tests run
// on a Windows development machine.

var errWindows = errors.New("controlling a running instance is only supported on Linux")

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

func terminate(int) error { return errWindows }

func hangup(int) error { return errWindows }

// copyOwner is a Unix concern; on Windows the file keeps whatever the
// filesystem gives it.
func copyOwner(src, dst string) error { return nil }
