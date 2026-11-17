package tools

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
)

// jsonUTF8 marshals v to compact JSON without HTML-escaping, so < > & and CJK
// stay raw UTF-8 (Go's json.Marshal turns < > & into \u00xx).
func jsonUTF8(v any) ([]byte, error) { return encodeJSON(v, false) }

// jsonUTF8Indent is jsonUTF8 with two-space indentation.
func jsonUTF8Indent(v any) ([]byte, error) { return encodeJSON(v, true) }

func encodeJSON(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// uEsc is the two-byte marker for a JSON unicode escape.
var uEsc = []byte(`\u`)

// normalizeJSON turns the \uXXXX escapes Chrome's DevTools emits for non-ASCII
// (every codepoint >= U+0080, and surrogate pairs for astral chars) back into
// raw UTF-8, so tools that forward a CDP payload verbatim (evaluate, raw_call,
// find, read grid) show 中文 instead of \u4e2d\u6587.
//
// It delegates to encoding/json/jsontext's Value.Format: with default options it
// rewrites JSON strings to their shortest encoding (which unescapes \uXXXX but
// leaves < > & literal) while preserving key order and number literals — no
// decode-into-any tree, no reflection. It is one canonicalizing pass over the
// bytes. Payloads without any "\u" (plain ASCII results) are returned unchanged,
// allocating nothing. Format mutates in place, so we format a copy and never
// touch the caller's buffer; invalid input falls back to returning raw.
func normalizeJSON(raw json.RawMessage) json.RawMessage {
	if !bytes.Contains(raw, uEsc) {
		return raw // nothing to unescape
	}
	v := jsontext.Value(append([]byte(nil), raw...)) // copy: Format mutates in place
	if err := v.Format(); err != nil {
		return raw
	}
	return json.RawMessage(v)
}
