// Package tools implements the MCP tool layer: action-aggregated tools built
// on the typed CDP client. The core pack: tabs (with per-tab viewport
// emulation via the `viewport` action), dialog, and window (real OS window
// move/resize).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"chrome-mcp/internal/cdp"
	"chrome-mcp/internal/filebox"
	"mcptransport"
)

// Core holds shared browser state: the DevTools connection and the tab the
// tools operate on by default.
type Core struct {
	hostPort string // Chrome DevTools endpoint host:port, for (re)dialing
	conn     *cdp.Conn
	browser  *cdp.Client // session "" (browser-level: Target.*)

	mu          sync.Mutex
	reconnectMu sync.Mutex // serialises (re)dial so concurrent calls reconnect once
	targetID    string     // current tab id
	sessionID   string     // current attached session
	dlg         *dialogState
	unsub       func()
	attached    map[string]string // targetId->sessionId for per-target ops (viewport) without stealing current

	consoles     map[string][]string // targetID -> buffered console/exception/log lines
	consolesMu   sync.Mutex
	consoleUnsub []func() // console-capture subscriptions for the current tab

	networks     map[string][]*netRec // targetID -> captured request records (append order)
	networksMu   sync.Mutex
	networkUnsub []func() // network-capture subscriptions for the current tab

	maxBytes int // bounds tool-result text size (config-adjustable); 0 => defaultMaxBytes

	files filebox.Box // jailed file store (screenshot saveAs, upload/download, storage_state); nil = disabled

	downloads     []*downloadRec // captured browser downloads (browser-level, append order)
	downloadsMu   sync.Mutex
	downloadUnsub []func()
}

type dialogState struct {
	Message string `json:"message"`
	Type    string `json:"type"` // alert|confirm|prompt|beforeunload
	Pending bool   `json:"pending"`
}

// NewCore dials the browser's DevTools endpoint and selects the first page
// target as the current tab.
func NewCore(ctx context.Context, hostPort string) (*Core, error) {
	conn, err := cdp.DialBrowser(ctx, hostPort)
	if err != nil {
		return nil, err
	}
	c := &Core{hostPort: hostPort, conn: conn, browser: conn.Client(""), attached: map[string]string{}, consoles: map[string][]string{}, networks: map[string][]*netRec{}, maxBytes: defaultMaxBytes}
	// Adopt the first page target, if any.
	if info, ok := c.firstPage(ctx); ok {
		_ = c.selectTarget(ctx, info.TargetID)
	}
	return c, nil
}

// SetFileRoot jails all file reads/writes to dir (see the filebox package).
// browserDir/browserVRoot map the virtual dir browserVRoot to the path the
// browser sees (use when Chrome runs in a container, e.g. /tmp/Downloads <->
// /downloads); leave browserDir empty for a same-filesystem browser.
// Without a root, file-writing tools (screenshot saveAs, upload/download, ...)
// are disabled.
func (c *Core) SetFileRoot(dir, browserDir, browserVRoot string) error {
	b, err := filebox.Open(dir, browserDir, browserVRoot)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.files = b
	c.mu.Unlock()
	return nil
}

// fileBox returns the jailed file store, or nil when none is configured.
func (c *Core) fileBox() filebox.Box {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.files
}

// live wraps a tool handler so the browser connection is verified before the
// handler runs. If Chrome has restarted (the DevTools WebSocket died) the
// connection is transparently re-dialed and the first page target re-adopted,
// so a browser restart no longer requires restarting the server: the next tool
// call after a restart reconnects.
func (c *Core) live(h mcptransport.Handler) mcptransport.Handler {
	return func(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
		if err := c.ensureLive(ctx); err != nil {
			return mcptransport.ResultError("browser unavailable: " + err.Error()), nil
		}
		return h(ctx, call)
	}
}

// ensureLive redials the browser endpoint if the current connection is closed.
func (c *Core) ensureLive(ctx context.Context) error {
	c.mu.Lock()
	alive := c.conn != nil && !c.conn.IsClosed()
	c.mu.Unlock()
	if alive {
		return nil
	}
	return c.reconnect(ctx)
}

// reconnect dials a fresh DevTools connection and re-establishes per-tab state.
// Serialised by reconnectMu so a burst of concurrent calls reconnects only once.
func (c *Core) reconnect(ctx context.Context) error {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	// Re-check: another caller may have reconnected while we waited.
	c.mu.Lock()
	connected := c.conn != nil && !c.conn.IsClosed()
	c.mu.Unlock()
	if connected {
		return nil
	}

	if c.conn != nil {
		_ = c.conn.Close()
	}
	conn, err := cdp.DialBrowser(ctx, c.hostPort)
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.unsub != nil {
		c.unsub()
	}
	c.conn = conn
	c.browser = conn.Client("")
	c.targetID = ""
	c.sessionID = ""
	c.dlg = nil
	c.attached = map[string]string{}
	c.mu.Unlock()

	c.resetCapture()

	// Re-adopt the first page target so page-scoped tools have a current tab.
	if info, ok := c.firstPage(ctx); ok {
		_ = c.selectTarget(ctx, info.TargetID)
	}
	log.Printf("cdp: reconnected to %s", c.hostPort)
	return nil
}

// resetCapture clears buffered console/network/download state after a reconnect.
// Old subscriptions live on the now-dead connection and stop firing on their
// own; buffers are wiped so capture starts fresh.
func (c *Core) resetCapture() {
	c.consolesMu.Lock()
	c.consoles = map[string][]string{}
	c.consolesMu.Unlock()
	c.networksMu.Lock()
	c.networks = map[string][]*netRec{}
	c.networksMu.Unlock()
	c.downloadsMu.Lock()
	c.downloads = nil
	c.downloadUnsub = nil
	c.downloadsMu.Unlock()
	c.mu.Lock()
	c.consoleUnsub = nil
	c.networkUnsub = nil
	c.mu.Unlock()
}

func (c *Core) firstPage(ctx context.Context) (*targetInfo, bool) {
	tabs, err := c.listTargets(ctx)
	if err != nil || len(tabs) == 0 {
		return nil, false
	}
	return &tabs[0], true
}

// page returns a client bound to the current tab's session.
func (c *Core) page() *cdp.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Client(c.sessionID)
}

// listTargets returns page-type targets.
func (c *Core) listTargets(ctx context.Context) ([]targetInfo, error) {
	var res struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := c.browser.RawCall(ctx, "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	pages := make([]targetInfo, 0, len(res.TargetInfos))
	for _, t := range res.TargetInfos {
		if t.Type == "page" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// selectTarget activates and attaches to a target, making it current.
func (c *Core) selectTarget(ctx context.Context, targetID string) error {
	if err := c.browser.RawCall(ctx, "Target.activateTarget", map[string]string{"targetId": targetID}, nil); err != nil {
		return err
	}
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.browser.RawCall(ctx, "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true}, &res); err != nil {
		return err
	}

	c.mu.Lock()
	if c.unsub != nil {
		c.unsub() // drop the previous tab's dialog subscription
	}
	c.targetID = targetID
	c.sessionID = res.SessionID
	c.dlg = nil
	c.mu.Unlock()

	// Track dialogs on this tab (Page.javascriptDialogOpening).
	page := c.conn.Client(res.SessionID)
	c.mu.Lock()
	c.unsub = page.Subscribe("Page.javascriptDialogOpening", func(params json.RawMessage) {
		var d dialogState
		_ = json.Unmarshal(params, &d)
		d.Pending = true
		c.mu.Lock()
		c.dlg = &d
		c.mu.Unlock()
	})
	c.mu.Unlock()

	// Enable Page so the dialog event fires.
	_ = page.RawCall(ctx, "Page.enable", nil, nil)
	// Capture console/exception/log output for this tab going forward.
	c.startConsoleCapture(ctx, page, targetID)
	// Capture network requests for this tab going forward.
	c.startNetworkCapture(ctx, page, targetID)
	return nil
}

// ensureAttached returns a client bound to targetID's session, attaching (and
// caching the session) if it is not already attached. It does not change the
// current tab, so a viewport override can be applied to a background tab.
func (c *Core) ensureAttached(ctx context.Context, targetID string) (*cdp.Client, error) {
	c.mu.Lock()
	if targetID == c.targetID && c.sessionID != "" {
		s := c.sessionID
		c.mu.Unlock()
		return c.conn.Client(s), nil
	}
	if sid, ok := c.attached[targetID]; ok {
		c.mu.Unlock()
		return c.conn.Client(sid), nil
	}
	c.mu.Unlock()

	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.browser.RawCall(ctx, "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true}, &res); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.attached[targetID] = res.SessionID
	c.mu.Unlock()
	return c.conn.Client(res.SessionID), nil
}

// ---------- tools ----------

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

// RegisterCore wires the core pack: tabs, dialog, window.
func RegisterCore(reg *mcptransport.Registry, core *Core) {
	must(reg.Register(tabsTool, core.live(core.tabs)))
	must(reg.Register(dialogTool, core.live(core.handleDialog)))
	must(reg.Register(windowTool, core.live(core.window)))
}

// RegisterBrowse wires the browse pack: pointer, input, scroll, wait, read,
// find. These operate on the current tab; tab switching lives in the core pack,
// so browse without core still acts on whatever tab Core already adopted.
func RegisterBrowse(reg *mcptransport.Registry, core *Core) {
	must(reg.Register(pointerTool, core.live(core.pointer)))
	must(reg.Register(inputTool, core.live(core.input)))
	must(reg.Register(scrollTool, core.live(core.scroll)))
	must(reg.Register(waitTool, core.live(core.wait)))
	must(reg.Register(readTool, core.live(core.read)))
	must(reg.Register(findTool, core.live(core.find)))
}

// Register wires both core and browse — the full default surface (convenience
// for callers/tests that do not toggle packs).
func Register(reg *mcptransport.Registry, core *Core) {
	RegisterCore(reg, core)
	RegisterBrowse(reg, core)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

var tabsTool = &mcptransport.Tool{
	Name:        "tabs",
	Description: "Manage browser tabs: list, new, close, select, nav.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action": {
				Type:        mcptransport.TypeString,
				Description: "list | new | close | select | nav | viewport",
				Enum:        []string{"list", "new", "close", "select", "nav", "viewport"},
			},
			"url":               {Type: mcptransport.TypeString, Description: "new: URL to open (default about:blank); nav: URL to navigate the current tab"},
			"targetId":          {Type: mcptransport.TypeString, Description: "close/select/viewport: target id"},
			"width":             {Type: mcptransport.TypeInteger, Description: "viewport: layout width in CSS px (required to set; omit to clear)"},
			"height":            {Type: mcptransport.TypeInteger, Description: "viewport: layout height in CSS px"},
			"deviceScaleFactor": {Type: mcptransport.TypeNumber, Description: "viewport: device pixel ratio (default 1)"},
			"mobile":            {Type: mcptransport.TypeBoolean, Description: "viewport: emulate mobile"},
		},
		Required: []string{"action"},
	},
}

func (c *Core) tabs(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action            string  `json:"action"`
		URL               string  `json:"url"`
		TargetID          string  `json:"targetId"`
		Width             int64   `json:"width"`
		Height            int64   `json:"height"`
		DeviceScaleFactor float64 `json:"deviceScaleFactor"`
		Mobile            bool    `json:"mobile"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}

	switch a.Action {
	case "list":
		pages, err := c.listTargets(ctx)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return jsonResult(pages)
	case "new":
		url := a.URL
		if url == "" {
			url = "about:blank"
		}
		var res struct {
			TargetID string `json:"targetId"`
		}
		if err := c.browser.RawCall(ctx, "Target.createTarget", map[string]string{"url": url}, &res); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		_ = c.selectTarget(ctx, res.TargetID)
		return jsonResult(map[string]string{"targetId": res.TargetID})
	case "close":
		if a.TargetID == "" {
			return mcptransport.ResultError("close needs targetId"), nil
		}
		if err := c.browser.RawCall(ctx, "Target.closeTarget", map[string]string{"targetId": a.TargetID}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		// Drop the closed target's cached session. If it was the current tab,
		// adopt another live page so current-bound ops (nav/dialog/window/
		// viewport) do not point at a dead target.
		c.mu.Lock()
		delete(c.attached, a.TargetID)
		wasCurrent := a.TargetID == c.targetID
		if wasCurrent {
			c.targetID = ""
			c.sessionID = ""
			if c.unsub != nil {
				c.unsub()
				c.unsub = nil
			}
			c.dlg = nil
		}
		c.mu.Unlock()
		if wasCurrent {
			if info, ok := c.firstPage(ctx); ok {
				_ = c.selectTarget(ctx, info.TargetID)
			}
		}
		return mcptransport.ResultText("closed " + a.TargetID), nil
	case "select":
		if a.TargetID == "" {
			return mcptransport.ResultError("select needs targetId"), nil
		}
		if err := c.selectTarget(ctx, a.TargetID); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText("selected " + a.TargetID), nil
	case "nav":
		if a.URL == "" {
			return mcptransport.ResultError("nav needs url"), nil
		}
		if err := c.page().RawCall(ctx, "Page.navigate", map[string]string{"url": a.URL}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText("navigated to " + a.URL), nil
	case "viewport":
		// Per-tab viewport emulation. Requires a targetId; a width+height
		// sets the override, omitting them clears it. Never steals the current tab.
		if a.TargetID == "" {
			return mcptransport.ResultError("viewport needs targetId"), nil
		}
		sess, err := c.ensureAttached(ctx, a.TargetID)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if a.Width <= 0 || a.Height <= 0 {
			if err := sess.RawCall(ctx, "Emulation.clearDeviceMetricsOverride", nil, nil); err != nil {
				return mcptransport.ResultError(err.Error()), nil
			}
			return mcptransport.ResultText("cleared viewport override on " + a.TargetID), nil
		}
		dsf := a.DeviceScaleFactor
		if dsf == 0 {
			dsf = 1
		}
		params := map[string]any{
			"width":             a.Width,
			"height":            a.Height,
			"deviceScaleFactor": dsf,
			"mobile":            a.Mobile,
		}
		if err := sess.RawCall(ctx, "Emulation.setDeviceMetricsOverride", params, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText(fmt.Sprintf("viewport of %s set to %dx%d", a.TargetID, a.Width, a.Height)), nil
	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
}

var dialogTool = &mcptransport.Tool{
	Name:        "dialog",
	Description: "Handle the current tab's open JS dialog: list, accept, dismiss.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action":     {Type: mcptransport.TypeString, Description: "list | accept | dismiss", Enum: []string{"list", "accept", "dismiss"}},
			"promptText": {Type: mcptransport.TypeString, Description: "accept: text for a prompt() dialog"},
		},
		Required: []string{"action"},
	},
}

func (c *Core) handleDialog(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action     string `json:"action"`
		PromptText string `json:"promptText"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}

	switch a.Action {
	case "list":
		c.mu.Lock()
		d := c.dlg
		c.mu.Unlock()
		if d == nil || !d.Pending {
			return mcptransport.ResultText("no open dialog"), nil
		}
		return jsonResult(d)
	case "accept":
		params := map[string]any{"accept": true}
		if a.PromptText != "" {
			params["promptText"] = a.PromptText
		}
		if err := c.page().RawCall(ctx, "Page.handleJavaScriptDialog", params, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		c.clearDialog()
		return mcptransport.ResultText("accepted"), nil
	case "dismiss":
		if err := c.page().RawCall(ctx, "Page.handleJavaScriptDialog", map[string]any{"accept": false}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		c.clearDialog()
		return mcptransport.ResultText("dismissed"), nil
	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
}

func (c *Core) clearDialog() {
	c.mu.Lock()
	if c.dlg != nil {
		c.dlg.Pending = false
	}
	c.mu.Unlock()
}

var windowTool = &mcptransport.Tool{
	Name:        "window",
	Description: "Move or resize the real OS browser window (Browser domain). info reads current bounds; set moves/resizes or changes window state.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action":   {Type: mcptransport.TypeString, Description: "info | set", Enum: []string{"info", "set"}},
			"targetId": {Type: mcptransport.TypeString, Description: "tab whose OS window to act on (default: current tab)"},
			"width":    {Type: mcptransport.TypeInteger},
			"height":   {Type: mcptransport.TypeInteger},
			"left":     {Type: mcptransport.TypeInteger},
			"top":      {Type: mcptransport.TypeInteger},
			"state":    {Type: mcptransport.TypeString, Description: "set: window state normal | maximized | minimized (overrides dims when set)", Enum: []string{"normal", "maximized", "minimized"}},
		},
		Required: []string{"action"},
	},
}

func (c *Core) window(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action   string `json:"action"`
		TargetID string `json:"targetId"`
		Width    *int64 `json:"width"`
		Height   *int64 `json:"height"`
		Left     *int64 `json:"left"`
		Top      *int64 `json:"top"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	c.mu.Lock()
	target := a.TargetID
	if target == "" {
		target = c.targetID
	}
	c.mu.Unlock()
	if target == "" {
		return mcptransport.ResultError("window needs a targetId (no current tab)"), nil
	}

	// Resolve the OS window that hosts this target.
	var wf struct {
		WindowID int64 `json:"windowId"`
		Bounds   struct {
			Left   int64  `json:"left"`
			Top    int64  `json:"top"`
			Width  int64  `json:"width"`
			Height int64  `json:"height"`
			State  string `json:"windowState"`
		} `json:"bounds"`
	}
	if err := c.browser.RawCall(ctx, "Browser.getWindowForTarget", map[string]string{"targetId": target}, &wf); err != nil {
		return mcptransport.ResultError(err.Error()), nil
	}

	switch a.Action {
	case "info":
		return jsonResult(wf)
	case "set":
		bounds := map[string]any{}
		if a.State != "" {
			// windowState takes precedence; do not mix it with dimensions.
			bounds["windowState"] = a.State
		} else {
			if a.Left != nil {
				bounds["left"] = *a.Left
			}
			if a.Top != nil {
				bounds["top"] = *a.Top
			}
			if a.Width != nil {
				bounds["width"] = *a.Width
			}
			if a.Height != nil {
				bounds["height"] = *a.Height
			}
		}
		if len(bounds) == 0 {
			return mcptransport.ResultError("set needs width/height/left/top or state"), nil
		}
		if err := c.browser.RawCall(ctx, "Browser.setWindowBounds",
			map[string]any{"windowId": wf.WindowID, "bounds": bounds}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		// Re-read so the caller sees the applied bounds.
		if err := c.browser.RawCall(ctx, "Browser.getWindowForTarget", map[string]string{"targetId": target}, &wf); err == nil {
			return jsonResult(wf)
		}
		return mcptransport.ResultText(fmt.Sprintf("window %d updated", wf.WindowID)), nil
	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
}

// jsonResult marshals v as indented JSON into a text result.
func jsonResult(v any) (*mcptransport.ToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcptransport.ResultError("marshal: " + err.Error()), nil
	}
	return mcptransport.ResultText(string(b)), nil
}
