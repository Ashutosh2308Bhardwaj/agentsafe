//go:build unix

package main

import (
	"os"
	"syscall"
)

// kill is kill -9 on this process: no deferred cleanup, nothing flushed. agentsafe doesn't need any. A signal
// sent to yourself is delivered asynchronously, so it never returns: this goroutine must not run one more line
// (such as logging the result it was killed before).
func kill() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}
