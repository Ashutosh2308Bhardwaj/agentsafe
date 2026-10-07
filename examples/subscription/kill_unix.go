//go:build unix

package main

import (
	"os"
	"syscall"
)

// kill is kill -9 on this process: no deferred cleanup, nothing flushed. agentsafe doesn't need any.
func kill() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) }
