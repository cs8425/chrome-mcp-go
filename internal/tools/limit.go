package tools

import (
	"fmt"
	"unicode/utf8"
)

// defaultMaxBytes caps the text any tool returns, guarding against a single
// huge result (innerText of a big page, JSON.stringify of a large object, a
// base64 data URL) blowing out the client context. Callers may override with a
// maxBytes argument. Incoming CDP reads are uncapped (SetReadLimit -1), so the
// outgoing tool result is where we must bound size.
const defaultMaxBytes = 50000

// capString truncates s to at most maxBytes on a rune boundary and, if it
// dropped anything, appends a marker naming the full size. maxBytes<=0 uses
// defaultMaxBytes.
func capString(s string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…[truncated: %d of %d bytes]", cut, len(s))
}

// SetMaxBytes sets the tool-result byte ceiling (config-adjustable). n<=0
// restores defaultMaxBytes.
func (c *Core) SetMaxBytes(n int) {
	if n <= 0 {
		n = defaultMaxBytes
	}
	c.maxBytes = n
}

// cap truncates s with the per-call requested limit if given (>0), else the
// Core's configured maxBytes (which itself falls back to defaultMaxBytes).
func (c *Core) cap(s string, requested int) string {
	if requested > 0 {
		return capString(s, requested)
	}
	return capString(s, c.maxBytes)
}
