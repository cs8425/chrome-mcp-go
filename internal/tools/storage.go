package tools

// The dev pack (cont.): storage. Cookies go through the CDP Network domain
// (authoritative, includes httpOnly); localStorage/sessionStorage go through
// the page's own JS, which is the same-origin view.

import (
	"context"
	"encoding/json"
	"fmt"

	"chrome-mcp/internal/cdp"
	"chrome-mcp/internal/filebox"
	"mcptransport"
)

type storageArgs struct {
	Type   string `json:"type"`
	Action string `json:"action"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Path   string `json:"path"`
}

var storageTool = &mcptransport.Tool{
	Name:        "storage",
	Description: "Read/write cookies (CDP Network) or localStorage/sessionStorage (page JS) for the current tab. type=cookie|local|session (default cookie); action=get|set|delete|clear (default get) plus state_save/state_load. set needs name+value; delete needs name; clear wipes all of that type. state_save/state_load dump/restore cookies + localStorage of the current origin to/from a JSON file in the file box (needs -root); path is the virtual file path. WARNING: the saved state may contain session tokens.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"type":   {Type: mcptransport.TypeString, Description: "cookie | local | session", Enum: []string{"cookie", "local", "session"}},
			"action": {Type: mcptransport.TypeString, Description: "get | set | delete | clear | state_save | state_load", Enum: []string{"get", "set", "delete", "clear", "state_save", "state_load"}},
			"name":   {Type: mcptransport.TypeString, Description: "cookie/storage key (set/delete)"},
			"value":  {Type: mcptransport.TypeString, Description: "value (set)"},
			"path":   {Type: mcptransport.TypeString, Description: "state_save/state_load: virtual JSON file path in the file box (e.g. /state.json)"},
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

	switch a.Action {
	case "state_save":
		return c.storageStateSave(ctx, a.Path)
	case "state_load":
		return c.storageStateLoad(ctx, a.Path)
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

// --- session state save/load -------------------------------------------------
// Playwright-compatible storage_state shape: {cookies, origins:[{origin,
// localStorage:[{name,value}]}]}. state_save dumps cookies + the current
// origin's localStorage to a file-box JSON; state_load restores them. Cookies
// are set browser-wide (domain based) so they apply regardless of the current
// tab; localStorage can only be written same-origin, so only entries matching
// the current page's origin are applied (others are reported as skipped).
// WARNING: the file may contain session tokens.

type stateKV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type stateCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

type stateOrigin struct {
	Origin       string    `json:"origin"`
	LocalStorage []stateKV `json:"localStorage"`
}

type storageState struct {
	Cookies []stateCookie `json:"cookies"`
	Origins []stateOrigin `json:"origins"`
}

func (c *Core) storageStateSave(ctx context.Context, path string) (*mcptransport.ToolResult, error) {
	if path == "" {
		return mcptransport.ResultError("state_save needs path"), nil
	}
	box := c.fileBox()
	if box == nil {
		return mcptransport.ResultError(filebox.ErrNoRoot.Error()), nil
	}
	page := c.page()

	var cr struct {
		Cookies []stateCookie `json:"cookies"`
	}
	if err := page.RawCall(ctx, "Network.getCookies", nil, &cr); err != nil {
		return mcptransport.ResultError(err.Error()), nil
	}
	st := storageState{}
	for _, ck := range cr.Cookies {
		if ck.Expires <= 0 {
			ck.Expires = 0 // session cookie -> omit expires
		}
		st.Cookies = append(st.Cookies, ck)
	}

	expr := `(function(){var o=window.localStorage;var out=[];for(var i=0;i<o.length;i++){var k=o.key(i);out.push({name:k,value:o.getItem(k)});}return {origin:location.origin,localStorage:out};})()`
	val, err := evalValue(ctx, page, expr)
	if err != nil {
		return mcptransport.ResultError("read localStorage: " + err.Error()), nil
	}
	var o stateOrigin
	if err := json.Unmarshal(val, &o); err != nil {
		return mcptransport.ResultError("parse localStorage: " + err.Error()), nil
	}
	if o.Origin != "" {
		st.Origins = []stateOrigin{o}
	}

	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		return mcptransport.ResultError(err.Error()), nil
	}
	if err := box.Write(ctx, path, data); err != nil {
		return mcptransport.ResultError("state_save: " + err.Error()), nil
	}
	vp, _ := filebox.Canonical(path)
	return mcptransport.ResultText(fmt.Sprintf("state saved: %s (%d cookie(s), %d localStorage item(s) for %s)", vp, len(st.Cookies), len(o.LocalStorage), o.Origin)), nil
}

func (c *Core) storageStateLoad(ctx context.Context, path string) (*mcptransport.ToolResult, error) {
	if path == "" {
		return mcptransport.ResultError("state_load needs path"), nil
	}
	box := c.fileBox()
	if box == nil {
		return mcptransport.ResultError(filebox.ErrNoRoot.Error()), nil
	}
	raw, err := box.Read(ctx, path)
	if err != nil {
		return mcptransport.ResultError("state_load: " + err.Error()), nil
	}
	var st storageState
	if err := json.Unmarshal(raw, &st); err != nil {
		return mcptransport.ResultError("parse state: " + err.Error()), nil
	}
	page := c.page()

	// Cookies: setCookies accepts only a subset of getCookies' fields.
	var cookies []map[string]any
	for _, ck := range st.Cookies {
		m := map[string]any{"name": ck.Name, "value": ck.Value}
		if ck.Domain != "" {
			m["domain"] = ck.Domain
			if ck.Path != "" {
				m["path"] = ck.Path
			}
		}
		if ck.Expires > 0 {
			m["expires"] = ck.Expires
		}
		if ck.HTTPOnly {
			m["httpOnly"] = true
		}
		if ck.Secure {
			m["secure"] = true
		}
		if ck.SameSite != "" {
			m["sameSite"] = ck.SameSite
		}
		cookies = append(cookies, m)
	}
	if len(cookies) > 0 {
		if err := page.RawCall(ctx, "Network.setCookies", map[string]any{"cookies": cookies}, nil); err != nil {
			return mcptransport.ResultError("set cookies: " + err.Error()), nil
		}
	}

	var curOrigin string
	if v, err := evalValue(ctx, page, "location.origin"); err == nil {
		_ = json.Unmarshal(v, &curOrigin)
	}
	applied, skipped := 0, 0
	for _, o := range st.Origins {
		if len(o.LocalStorage) == 0 {
			continue
		}
		if o.Origin != curOrigin {
			skipped += len(o.LocalStorage)
			continue
		}
		if err := setLocalStorageBatch(ctx, page, o.LocalStorage); err != nil {
			return mcptransport.ResultError("localStorage: " + err.Error()), nil
		}
		applied += len(o.LocalStorage)
	}

	msg := fmt.Sprintf("state loaded: %d cookie(s), %d localStorage item(s)", len(cookies), applied)
	if skipped > 0 {
		msg += fmt.Sprintf("; %d localStorage item(s) skipped (origin mismatch — navigate to that origin then load)", skipped)
	}
	return mcptransport.ResultText(msg), nil
}

// setLocalStorageBatch writes many items in a single JS round trip.
func setLocalStorageBatch(ctx context.Context, page *cdp.Client, items []stateKV) error {
	var b []byte
	b = append(b, '[')
	for i, kv := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		b = append(b, jsString(kv.Name)...)
		b = append(b, ',')
		b = append(b, jsString(kv.Value)...)
		b = append(b, ']')
	}
	b = append(b, ']')
	expr := `(function(){var d=` + string(b) + `;for(var i=0;i<d.length;i++){window.localStorage.setItem(d[i][0],d[i][1]);}return true;})()`
	_, err := evalValue(ctx, page, expr)
	return err
}
