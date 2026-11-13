package tools

// The dev pack (off by default in the spec): powerful debugging tools. P3a
// ships evaluate; console/network/emulate/storage/cdp_call follow.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mcptransport"
)

var evaluateTool = &mcptransport.Tool{
	Name:        "evaluate",
	Description: "Evaluate a JavaScript expression in the current tab and return its value as JSON. Powerful: runs arbitrary JS in the page. To get a promise's result, return the promise and set await=true (the flag awaits it; a bare top-level `await` keyword is not reliably parsed). The result is truncated at maxBytes (default 50000).",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"expression": {Type: mcptransport.TypeString, Description: "JavaScript expression to evaluate"},
			"await":      {Type: mcptransport.TypeBoolean, Description: "await the returned promise (resolve value, or reject -> error)"},
			"maxBytes":   {Type: mcptransport.TypeNumber, Description: "truncate result to this many bytes (default 50000)"},
			"timeout":    {Type: mcptransport.TypeNumber, Description: "max seconds to wait, esp. with await=true (default 30)"},
		},
		Required: []string{"expression"},
	},
}

func (c *Core) evaluate(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Expression string  `json:"expression"`
		Await      bool    `json:"await"`
		MaxBytes   int     `json:"maxBytes"`
		Timeout    float64 `json:"timeout"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	if a.Expression == "" {
		return mcptransport.ResultError("evaluate needs expression"), nil
	}
	if a.Timeout <= 0 {
		a.Timeout = 30
	}
	// Bound the call so a never-resolving awaited promise cannot hang the server.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(a.Timeout*float64(time.Second)))
		defer cancel()
	}
	page := c.page()
	var res struct {
		Result struct {
			Type        string          `json:"type"`
			Subtype     string          `json:"subtype"`
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := page.RawCall(ctx, "Runtime.evaluate", map[string]any{
		"expression": a.Expression, "returnByValue": true, "awaitPromise": a.Await,
	}, &res); err != nil {
		if ctx.Err() != nil {
			return mcptransport.ResultError(fmt.Sprintf("evaluate timed out after %gs", a.Timeout)), nil
		}
		return mcptransport.ResultError(err.Error()), nil
	}
	if res.ExceptionDetails != nil {
		txt := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception.Description != "" {
			txt = res.ExceptionDetails.Exception.Description
		}
		return mcptransport.ResultError("eval error: " + c.cap(txt, a.MaxBytes)), nil
	}
	if len(res.Result.Value) > 0 {
		return mcptransport.ResultText(c.cap(string(res.Result.Value), a.MaxBytes)), nil
	}
	// undefined, or a non-serializable object with no JSON value.
	if res.Result.Description != "" {
		return mcptransport.ResultText(c.cap(res.Result.Description, a.MaxBytes)), nil
	}
	return mcptransport.ResultText("<" + res.Result.Type + ">"), nil
}

// RegisterDev wires the dev-pack tools (off by default in the spec) into a
// registry. main gates this on config; tests and the current build enable it.
func RegisterDev(reg *mcptransport.Registry, core *Core) {
	must(reg.Register(evaluateTool, core.live(core.evaluate)))
	must(reg.Register(consoleTool, core.live(core.console)))
	must(reg.Register(networkTool, core.live(core.network)))
	must(reg.Register(emulateTool, core.live(core.emulate)))
	must(reg.Register(storageTool, core.live(core.storage)))
	must(reg.Register(cdpCallTool, core.live(core.cdpCall)))
}
