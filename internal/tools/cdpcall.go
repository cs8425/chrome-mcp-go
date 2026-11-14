package tools

// The dev pack (cont.): raw_call. The escape hatch — send any raw Chrome
// DevTools Protocol method. On by default in the dev pack; a policy can drop
// the whole dev pack if this is too permissive.

import (
	"context"
	"encoding/json"

	"mcptransport"
)

var cdpCallTool = &mcptransport.Tool{
	Name:        "raw_call",
	Description: "Escape hatch: call any raw Chrome DevTools Protocol method. params is the method's JSON arguments object (optional). Set browser=true to send at the browser endpoint instead of the current tab's session. Returns the raw result JSON (truncated).",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"method":  {Type: mcptransport.TypeString, Description: "CDP method name, e.g. Runtime.evaluate or Page.getLayoutMetrics"},
			"params":  {Type: mcptransport.TypeObject, Description: "method parameters object (optional)"},
			"browser": {Type: mcptransport.TypeBoolean, Description: "call on the browser endpoint (no session) instead of the current tab"},
		},
		Required: []string{"method"},
	},
}

func (c *Core) cdpCall(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Browser bool            `json:"browser"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	if a.Method == "" {
		return mcptransport.ResultError("raw_call needs method"), nil
	}
	var params any
	if len(a.Params) > 0 {
		var m map[string]any
		if err := json.Unmarshal(a.Params, &m); err != nil {
			return mcptransport.ResultError("params must be an object: " + err.Error()), nil
		}
		params = m
	}
	var res json.RawMessage
	var err error
	if a.Browser {
		err = c.browser.Load().RawCall(ctx, a.Method, params, &res)
	} else {
		err = c.page().RawCall(ctx, a.Method, params, &res)
	}
	if err != nil {
		return mcptransport.ResultError(err.Error()), nil
	}
	if len(res) == 0 || string(res) == "{}" {
		return mcptransport.ResultText("{}"), nil
	}
	return mcptransport.ResultText(c.cap(string(res), 0)), nil
}
