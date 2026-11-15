//go:build !windows

package main

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

// startDiagnostics dumps every goroutine stack to stderr when SIGUSR1 is
// received, WITHOUT killing the process. Use it to pinpoint exactly where a hung
// tool call is parked (a mutex, a channel, or a CDP select) when the call
// timeouts don't fire. POSIX only; see signals_windows.go for the no-op.
func startDiagnostics() {
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGUSR1)
		for range sigCh {
			buf := make([]byte, 1<<20)
			if n := runtime.Stack(buf, true); n > 0 {
				log.Printf("goroutine dump (SIGUSR1):\n%s", buf[:n])
			}
		}
	}()
}
