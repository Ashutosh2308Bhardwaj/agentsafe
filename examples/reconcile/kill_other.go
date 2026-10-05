//go:build !unix

package main

// killHookFn is a no-op where SIGKILL isn't available (Windows): the crash harness is Unix-only.
func killHookFn(string) {}
