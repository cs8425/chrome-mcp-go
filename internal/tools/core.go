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
	"sync/atomic"
	"time"

	"chrome-mcp/internal/cdp"
	"chrome-mcp/internal/filebox"
	"mcptransport"
)

// Core holds shared browser state: the DevTools connection and the tab the
// tools operate on by default.
type Core struct {
	hostPort string // Chrome DevTools endpoint host:port, for (re)dialing
	conn     atomic.Pointer[cdp.Conn]
	browser  atomic.Pointer[cdp.Client] // session "" (browser-level: Target.*)

	mu               sync.Mutex
	reconnectMu      sync.Mutex   // serialises (re)dial so concurrent calls reconnect once
	callTimeout      atomic.Int64 // per-CDP-call deadline in ns (0 = none); applied to every conn
	targetMu         sync.Mutex   // serialises structural tab ops (close/select/new) so they cannot interleave
	activateOnSelect bool         // when true, select/new raise the window (Target.activateTarget); default false = no focus steal
	targetID         string       // current tab id
	sessionID        string       // current attached session
	dlg              *dialogState
	unsub            func()
	attached         map[string]string // targetId->sessionId for per-target ops (viewport) without stealing current

	consoles    map[string][]string // targetID -> buffered console/exception/log lines
	consolesMu  sync.Mutex
	consoleSubs map[string][]func() // targetID -> console capture subs (alive for the tab's lifetime)

	networks    map[string][]*netRec // targetID -> captured request records (append order)
	networksMu  sync.Mutex
	networkSubs map[string][]func() // targetID -> network capture subs (alive for the tab's lifetime)

	targetUnsub func() // browser-level Target.targetCreated watcher (capture new tabs from birth)

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
	c := &Core{hostPort: hostPort, attached: map[string]string{}, consoles: map[string][]string{}, networks: map[string][]*netRec{}, consoleSubs: map[string][]func(){}, networkSubs: map[string][]func(){}, maxBytes: defaultMaxBytes}
	c.conn.Store(conn)
	c.browser.Store(conn.Client(""))
	// Adopt the first page target, if any (never raise the window on startup).
	if info, ok := c.firstPage(ctx); ok {
		_ = c.selectTarget(ctx, info.TargetID, false)
	}
	// Capture tabs the page opens itself (popups, target=_blank) from birth.
	c.startTargetWatch(ctx)
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

// Close releases resources held by the core: the file box handle and the browser
// connection. On Windows an open file-box handle keeps the jailed directory from
// being deleted, so owners (and tests running over a t.TempDir) must Close it.
func (c *Core) Close() {
	if b := c.fileBox(); b != nil {
		_ = b.Close()
	}
	if cc := c.conn.Load(); cc != nil && !cc.IsClosed() {
		_ = cc.Close()
	}
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
	cc := c.conn.Load()
	alive := cc != nil && !cc.IsClosed()
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
	cc := c.conn.Load()
	connected := cc != nil && !cc.IsClosed()
	c.mu.Unlock()
	if connected {
		return nil
	}

	if cc != nil {
		_ = cc.Close()
	}
	conn, err := cdp.DialBrowser(ctx, c.hostPort)
	if err != nil {
		return err
	}
	conn.SetCallTimeout(time.Duration(c.callTimeout.Load()))

	c.mu.Lock()
	if c.unsub != nil {
		c.unsub()
	}
	c.conn.Store(conn)
	c.browser.Store(conn.Client(""))
	c.targetID = ""
	c.sessionID = ""
	c.dlg = nil
	c.attached = map[string]string{}
	c.mu.Unlock()

	c.resetCapture()
	c.startTargetWatch(ctx)

	// Re-adopt the first page target so page-scoped tools have a current tab
	// (do not raise the window: reconnecting must not steal focus).
	if info, ok := c.firstPage(ctx); ok {
		_ = c.selectTarget(ctx, info.TargetID, false)
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
	c.consoleSubs = map[string][]func(){}
	c.networkSubs = map[string][]func(){}
	c.targetUnsub = nil
	c.mu.Unlock()
}

// forgetBuffers frees a closed target's capture: buffered console lines, network
// records, and its per-target subscriptions (the cached session is dropped by
// the caller). Must NOT be called while holding c.mu — it takes c.mu for the
// subscription maps.
func (c *Core) forgetBuffers(targetID string) {
	c.consolesMu.Lock()
	delete(c.consoles, targetID)
	c.consolesMu.Unlock()
	c.networksMu.Lock()
	delete(c.networks, targetID)
	c.networksMu.Unlock()

	c.mu.Lock()
	cu := c.consoleSubs[targetID]
	nu := c.networkSubs[targetID]
	delete(c.consoleSubs, targetID)
	delete(c.networkSubs, targetID)
	c.mu.Unlock()
	for _, u := range cu {
		u()
	}
	for _, u := range nu {
		u()
	}
}

func (c *Core) firstPage(ctx context.Context) (*targetInfo, bool) {
	tabs, err := c.listTargets(ctx)
	if err != nil || len(tabs) == 0 {
		return nil, false
	}
	return &tabs[0], true
}

// startTargetWatch begins capturing console/network for page targets as they are
// created, so tabs the page opens itself (popups, target=_blank, OAuth windows)
// are captured from birth — not only once the agent selects them. Idempotent.
func (c *Core) startTargetWatch(ctx context.Context) {
	c.mu.Lock()
	if c.targetUnsub != nil {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	un := c.browser.Load().Subscribe("Target.targetCreated", func(params json.RawMessage) {
		var e struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(params, &e) != nil || e.TargetInfo.Type != "page" || e.TargetInfo.TargetID == "" {
			return
		}
		tid := e.TargetInfo.TargetID
		// Never block the read loop: attach/enable round-trips run async.
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c.captureTarget(bg, tid)
		}()
	})
	c.mu.Lock()
	c.targetUnsub = un
	c.mu.Unlock()

	// Ask the browser to emit Target.targetCreated events.
	_ = c.browser.Load().RawCall(ctx, "Target.setDiscoverTargets", map[string]bool{"discover": true}, nil)
}

// captureTarget attaches to targetID (without changing the current tab) and
// starts console + network capture. Safe to call repeatedly; a no-op once the
// target is being captured. Used to prime every visible tab on `tabs list` and
// to pick up freshly created page targets.
func (c *Core) captureTarget(ctx context.Context, targetID string) {
	c.mu.Lock()
	_, active := c.consoleSubs[targetID]
	c.mu.Unlock()
	if active {
		return
	}
	sess, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return // target gone or attach failed
	}
	c.startConsoleCapture(ctx, sess, targetID)
	c.startNetworkCapture(ctx, sess, targetID)
}

// page returns a client bound to the current tab's session.
func (c *Core) page() *cdp.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Load().Client(c.sessionID)
}

// listTargets returns page-type targets.
func (c *Core) listTargets(ctx context.Context) ([]targetInfo, error) {
	var res struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := c.browser.Load().RawCall(ctx, "Target.getTargets", nil, &res); err != nil {
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

// SetActivateOnSelect controls whether select/new raise the browser window to
// the foreground (Target.activateTarget). Default false: attaching in the
// background never steals the user's OS focus.
func (c *Core) SetActivateOnSelect(on bool) { c.activateOnSelect = on }

// selectTarget attaches to a target, making it current. Attaching alone does
// NOT raise the OS window, so it never steals focus. Pass activate=true only
// when the caller explicitly wants the tab brought to the foreground
// (Target.activateTarget raises the window and focuses it).
func (c *Core) selectTarget(ctx context.Context, targetID string, activate bool) error {
	if activate {
		if err := c.browser.Load().RawCall(ctx, "Target.activateTarget", map[string]string{"targetId": targetID}, nil); err != nil {
			return err
		}
	}
	// Attach once; the session is cached and reused across selects so the capture
	// subscriptions below bind to a stable session.
	sess, err := c.ensureAttached(ctx, targetID)
	if err != nil {
		return err
	}

	// Dialogs are tracked for the current tab only.
	c.mu.Lock()
	if c.unsub != nil {
		c.unsub() // drop the previous tab's dialog subscription
	}
	c.targetID = targetID
	c.sessionID = sess.SessionID()
	c.dlg = nil
	c.unsub = sess.Subscribe("Page.javascriptDialogOpening", func(params json.RawMessage) {
		var d dialogState
		_ = json.Unmarshal(params, &d)
		d.Pending = true
		c.mu.Lock()
		c.dlg = &d
		c.mu.Unlock()
	})
	c.mu.Unlock()

	// Enable Page so the dialog event fires.
	_ = sess.RawCall(ctx, "Page.enable", nil, nil)
	// Capture console + network for this tab. Idempotent and persistent: the tab
	// keeps being captured even after it stops being the current tab.
	c.startConsoleCapture(ctx, sess, targetID)
	c.startNetworkCapture(ctx, sess, targetID)
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
		return c.conn.Load().Client(s), nil
	}
	if sid, ok := c.attached[targetID]; ok {
		c.mu.Unlock()
		return c.conn.Load().Client(sid), nil
	}
	c.mu.Unlock()

	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.browser.Load().RawCall(ctx, "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true}, &res); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.attached[targetID] = res.SessionID
	c.mu.Unlock()
	return c.conn.Load().Client(res.SessionID), nil
}

// SetCDPCallTimeout sets the per-CDP-call deadline (0 disables) on the current
// connection and on every future (re)connection.
func (c *Core) SetCDPCallTimeout(d time.Duration) {
	c.callTimeout.Store(int64(d))
	if cc := c.conn.Load(); cc != nil {
		cc.SetCallTimeout(d)
	}
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
	must(reg.Register(interactTool, core.live(core.interact)))
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
	Description: "Manage browser tabs: list, new, close, select, nav. select/new attach to a tab in the background and do NOT raise it (no OS focus steal); pass bringToFront=true ONLY when the user explicitly wants a tab brought forward — in a HEADED browser that raises the Chrome window and steals the user's focus, so avoid it otherwise.",
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
			"bringToFront":      {Type: mcptransport.TypeBoolean, Description: "new/select: also raise the window to the foreground (default false). In a HEADED browser this raises the Chrome window and steals the user's OS focus — set true ONLY when the user explicitly asks to bring the tab to front."},
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
		BringToFront      bool    `json:"bringToFront"`
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
		// Ensure every visible tab is being captured (covers tabs that existed
		// before we started watching), then report.
		for _, p := range pages {
			c.captureTarget(ctx, p.TargetID)
		}
		return jsonResult(pages)
	case "new":
		c.targetMu.Lock()
		defer c.targetMu.Unlock()
		url := a.URL
		if url == "" {
			url = "about:blank"
		}
		wantForeground := a.BringToFront || c.activateOnSelect
		params := map[string]any{"url": url}
		if !wantForeground {
			// Target.createTarget focuses the new tab and raises the window unless
			// told otherwise; focus:false opens the tab (still the active tab in
			// its window, so it renders and stays screenshotable) while leaving the
			// browser window's OS focus exactly where it was. Foreground is requested
			// explicitly via bringToFront / activate_on_select, which omit focus:false
			// and take the default (raise) path.
			params["focus"] = false
		}
		var res struct {
			TargetID string `json:"targetId"`
		}
		if err := c.browser.Load().RawCall(ctx, "Target.createTarget", params, &res); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		_ = c.selectTarget(ctx, res.TargetID, wantForeground)
		return jsonResult(map[string]string{"targetId": res.TargetID})
	case "close":
		c.targetMu.Lock()
		defer c.targetMu.Unlock()
		if a.TargetID == "" {
			return mcptransport.ResultError("close needs targetId"), nil
		}
		if err := c.browser.Load().RawCall(ctx, "Target.closeTarget", map[string]string{"targetId": a.TargetID}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		// Free buffered capture for the closed tab (console lines + network
		// records) so closing tabs does not leak per-target entries.
		c.forgetBuffers(a.TargetID)
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
				_ = c.selectTarget(ctx, info.TargetID, false)
			}
		}
		return mcptransport.ResultText("closed " + a.TargetID), nil
	case "select":
		c.targetMu.Lock()
		defer c.targetMu.Unlock()
		if a.TargetID == "" {
			return mcptransport.ResultError("select needs targetId"), nil
		}
		if err := c.selectTarget(ctx, a.TargetID, a.BringToFront || c.activateOnSelect); err != nil {
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
	Description: "Move or resize the real OS browser window (Browser domain). info reads current bounds (safe); set moves/resizes/changes window state and acts on the REAL window — in a HEADED browser this raises/alters the window the user is looking at, so avoid set unless the user requests window changes (prefer tabs action=viewport for rendering size).",
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
	if err := c.browser.Load().RawCall(ctx, "Browser.getWindowForTarget", map[string]string{"targetId": target}, &wf); err != nil {
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
		if err := c.browser.Load().RawCall(ctx, "Browser.setWindowBounds",
			map[string]any{"windowId": wf.WindowID, "bounds": bounds}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		// Re-read so the caller sees the applied bounds.
		if err := c.browser.Load().RawCall(ctx, "Browser.getWindowForTarget", map[string]string{"targetId": target}, &wf); err == nil {
			return jsonResult(wf)
		}
		return mcptransport.ResultText(fmt.Sprintf("window %d updated", wf.WindowID)), nil
	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
}

// jsonResult marshals v as indented JSON into a text result.
func jsonResult(v any) (*mcptransport.ToolResult, error) {
	b, err := jsonUTF8Indent(v)
	if err != nil {
		return mcptransport.ResultError("marshal: " + err.Error()), nil
	}
	return mcptransport.ResultText(string(b)), nil
}
