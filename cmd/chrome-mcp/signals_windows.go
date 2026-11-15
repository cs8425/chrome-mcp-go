//go:build windows

package main

// startDiagnostics is a no-op on Windows: SIGUSR1 — used on POSIX to dump
// goroutine stacks to stderr — does not exist there. Everything else in the
// server is portable, so only this diagnostic differs per platform.
func startDiagnostics() {}
