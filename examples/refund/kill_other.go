//go:build !unix

package main

import "os"

// kill ends the process at once where SIGKILL isn't available (Windows): no deferred cleanup either.
func kill() { os.Exit(137) }
