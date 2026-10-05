//go:build unix

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// killHookFn SIGKILLs this process at a named hook point: a real crash, no defers, no flushes.
// Runner points: after_model_call, after_model_logged, before_tool_executed, after_tool_executed,
// after_result_logged, approval_requested. Gateway point: gateway_charged (money moved, nobody told).
var killSeen int

func killHookFn(p string) {
	at, nth := os.Getenv("KILL_AT"), 1
	if n, err := strconv.Atoi(os.Getenv("KILL_NTH")); err == nil {
		nth = n
	}
	if p == at {
		if killSeen++; killSeen == nth {
			fmt.Printf("    💀 kill -9 at %s (occurrence %d)\n", p, nth)
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL) // if this fails we're about to be wrong anyway
		}
	}
}
