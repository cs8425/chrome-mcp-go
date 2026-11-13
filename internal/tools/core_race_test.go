package tools

import (
	"sync"
	"testing"
)

// TestCoreConnBrowserAtomic hammers the atomic conn/browser pointers from many
// goroutines while another goroutine swaps them, keeping `go test -race` clean.
// Before these Core fields were atomic (they were plain *cdp.* pointers read by
// tool handlers while a reconnect overwrote them) this exact pattern raced; if
// someone reverts them to plain pointers, the race detector flags this test.
func TestCoreConnBrowserAtomic(t *testing.T) {
	var c Core
	var wg sync.WaitGroup
	const readers, spins = 8, 5000

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < spins; j++ {
				_ = c.browser.Load()
				if cc := c.conn.Load(); cc != nil {
					_ = cc.IsClosed()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < spins; j++ {
			c.browser.Store(nil)
			c.conn.Store(nil)
		}
	}()
	wg.Wait()
}
