package tools

// The dev pack (cont.): storage. Cookies go through the CDP Network domain
// (authoritative, includes httpOnly); localStorage/sessionStorage go through
// the page's own JS, which is the same-origin view.

import (
	"context"
	"encoding/json"

	"chrome-mcp/internal/cdp"
	"mcptransport"
)

type storageArgs struct {
	Type   string `json:"type"`
	Action string `json:"action"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

var storageTool = &mcptransport.Tool{
	Name:        "storage",
	Description: "Read/write cookies (CDP Network) or localStorage/sessionStorage (page JS) for the current tab. type=cookie|local|session (default cookie); action=get|set|delete|clear (default get). set needs name+value; delete needs name; clear wipes all of that type.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"type":   {Type: mcptransport.TypeString, Description: "cookie | local | session", Enum: []string{"cookie", "local", "session"}},
			"action": {Type: mcptransport.TypeString, Description: "get | set | delete | clear", Enum: []string{"get", "set", "delete", "clear"}},
			"name":   {Type: mcptransport.TypeString, Description: "cookie/storage key (set/delete)"},
			"value":  {Type: mcptransport.TypeString, Description: "value (set)"},
		},
	},
}

func (c *Core) storage(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a storageArgs
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	if a.Type == "" {
		a.Type = "cookie"
	}
	if a.Action == "" {
		a.Action = "get"
	}
	page := c.page()
	switch a.Type {
	case "cookie":
		return c.storageCookie(ctx, page, &a)
	case "local", "session":
		return c.storageWeb(ctx, page, &a)
	default:
		return mcptransport.ResultError("unknown type: " + a.Type), nil
	}
}

func (c *Core) storageCookie(ctx context.Context, page *cdp.Client, a *storageArgs) (*mcptransport.ToolResult, error) {
	url, _ := c.currentURL(ctx)
	switch a.Action {
	case "get":
		var res struct {
			Cookies []struct {
				Name     string `json:"name"`
				Value    string `json:"value"`
				Domain   string `json:"domain"`
				Path     string `json:"path"`
				HTTPOnly bool   `json:"httpOnly"`
				Secure   bool   `json:"secure"`
			} `json:"cookies"`
		}
		if err := page.RawCall(ctx, "Network.getCookies", map[string]any{"urls": []string{url}}, &res); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if len(res.Cookies) == 0 {
			return mcptransport.ResultText("(no cookies)"), nil
		}
		j, _ := json.Marshal(res.Cookies)
		return mcptransport.ResultText(c.cap(string(j), 0)), nil
	case "set":
		if a.Name == "" {
			return mcptransport.ResultError("set needs name"), nil
		}
		if err := page.RawCall(ctx, "Network.setCookie", map[string]any{"name": a.Name, "value": a.Value, "url": url}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText("cookie set: " + a.Name), nil
	case "delete":
		if a.Name == "" {
			return mcptransport.ResultError("delete needs name"), nil
		}
		if err := page.RawCall(ctx, "Network.deleteCookies", map[string]any{"name": a.Name, "url": url}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText("cookie deleted: " + a.Name), nil
	case "clear":
		if err := page.RawCall(ctx, "Network.clearBrowserCookies", nil, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText("all cookies cleared"), nil
	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
}

func (c *Core) storageWeb(ctx context.Context, page *cdp.Client, a *storageArgs) (*mcptransport.ToolResult, error) {
	obj := "localStorage"
	if a.Type == "session" {
		obj = "sessionStorage"
	}
	switch a.Action {
	case "get":
		expr := `(function(){var o=window.` + obj + `;var out=[];for(var i=0;i<o.length;i++){var k=o.key(i);out.push([k,o.getItem(k)]);}return out;})()`
		val, err := evalValue(ctx, page, expr)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		s := string(val)
		if s == "" || s == "[]" || s == "null" {
			return mcptransport.ResultText("(no items)"), nil
		}
		return mcptransport.ResultText(c.cap(s, 0)), nil
	case "set":
		if a.Name == "" {
			return mcptransport.ResultError("set needs name"), nil
		}
		expr := `(function(){window.` + obj + `.setItem(` + jsString(a.Name) + `,` + jsString(a.Value) + `);return true;})()`
		if _, err := evalValue(ctx, page, expr); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText(obj + " set: " + a.Name), nil
	case "delete":
		if a.Name == "" {
			return mcptransport.ResultError("delete needs name"), nil
		}
		expr := `(function(){window.` + obj + `.removeItem(` + jsString(a.Name) + `);return true;})()`
		if _, err := evalValue(ctx, page, expr); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText(obj + " deleted: " + a.Name), nil
	case "clear":
		expr := `(function(){window.` + obj + `.clear();return true;})()`
		if _, err := evalValue(ctx, page, expr); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText(obj + " cleared"), nil
	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
}
