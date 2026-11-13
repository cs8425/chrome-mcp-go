package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"mcptransport"

	"github.com/coder/websocket"
)

// startFakeCDP is the same minimal CDP endpoint as the cdp package test,
// inlined here because test helpers are not importable across packages.
func startFakeCDP(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	var wsURL string
	var closedMu sync.Mutex
	closed := map[string]bool{}

	mux.HandleFunc("/json/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"WebsocketDebuggerUrl": wsURL})
	})
	mux.HandleFunc("/devtools/browser/test", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID        int64           `json:"id"`
				SessionID string          `json:"sessionId"`
				Method    string          `json:"method"`
				Params    json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				continue
			}
			out := map[string]any{"id": req.ID}
			switch req.Method {
			case "Target.getTargets":
				all := []map[string]any{
					{"targetId": "t1", "type": "page", "title": "A", "url": "http://a"},
					{"targetId": "t2", "type": "iframe"},
					{"targetId": "t3", "type": "page", "title": "C", "url": "http://c"},
				}
				var visible []map[string]any
				for _, ti := range all {
					id, _ := ti["targetId"].(string)
					closedMu.Lock()
					isClosed := closed[id]
					closedMu.Unlock()
					if isClosed {
						continue
					}
					visible = append(visible, ti)
				}
				out["result"] = map[string]any{"targetInfos": visible}
			case "Target.closeTarget":
				var p struct {
					TargetID string `json:"targetId"`
				}
				_ = json.Unmarshal(req.Params, &p)
				closedMu.Lock()
				closed[p.TargetID] = true
				closedMu.Unlock()
				out["result"] = map[string]any{"success": true}
			case "Target.attachToTarget":
				out["result"] = map[string]any{"sessionId": "S1"}
			case "Target.createTarget":
				out["result"] = map[string]any{"targetId": "new1"}
			case "Browser.getWindowForTarget":
				out["result"] = map[string]any{"windowId": 42, "bounds": map[string]any{
					"left": 0, "top": 0, "width": 1265, "height": 1387, "windowState": "normal",
				}}
			case "DOM.getDocument":
				out["result"] = map[string]any{"root": map[string]any{"nodeId": 5}}
			case "DOM.querySelector":
				var qp struct {
					Selector string `json:"selector"`
				}
				_ = json.Unmarshal(req.Params, &qp)
				nid := int64(5) // content box below centers at (20,20)
				if strings.Contains(qp.Selector, "missing") {
					nid = 0 // selector matched nothing
				}
				out["result"] = map[string]any{"nodeId": nid}
			case "DOM.getBoxModel":
				out["result"] = map[string]any{"model": map[string]any{"content": []float64{10, 10, 30, 10, 30, 30, 10, 30}}}
			case "Page.getLayoutMetrics":
				out["result"] = map[string]any{"cssVisualViewport": map[string]any{"width": 800, "height": 600}}
			case "DOM.getOuterHTML":
				out["result"] = map[string]any{"outerHTML": "<html><body>hello world</body></html>"}
			case "Page.captureScreenshot":
				out["result"] = map[string]any{"data": "iVBORw0KGgo="}
			case "Target.getTargetInfo":
				out["result"] = map[string]any{"targetInfo": map[string]any{"url": "https://example.com/abc", "title": "t"}}
			case "Runtime.evaluate":
				p := string(req.Params)
				switch {
				case strings.Contains(p, "headers"):
					out["result"] = map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{
						"headers": []string{"Name", "Qty"},
						"rows":    [][]string{{"Apple", "3"}, {"Pear", "5"}},
					}}}
				case strings.Contains(p, "el.value"):
					out["result"] = map[string]any{"result": map[string]any{"type": "string", "value": "field-val-42"}}
				case strings.Contains(p, "score"):
					out["result"] = map[string]any{"result": map[string]any{"type": "object", "value": []map[string]any{
						{"score": 5, "role": "button", "name": "Save changes", "selector": "#save"},
					}}}
				case strings.Contains(p, "EVALTEST"):
					out["result"] = map[string]any{"result": map[string]any{"type": "number", "value": 42}}
				default:
					out["result"] = map[string]any{"result": map[string]any{"type": "string", "value": "visible page text"}}
				}
			default:
				out["result"] = map[string]any{}
			}
			rb, _ := json.Marshal(out)
			if err := c.Write(ctx, websocket.MessageText, rb); err != nil {
				return
			}
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	wsURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/devtools/browser/test"
	return strings.TrimPrefix(srv.URL, "http://")
}

func callTool(t *testing.T, ctx context.Context, reg *mcptransport.Registry, name, args string) *mcptransport.ToolResult {
	t.Helper()
	res, err := reg.Dispatch(ctx, &mcptransport.Call{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("dispatch %s: %v", name, err)
	}
	return res
}

func firstText(t *testing.T, res *mcptransport.ToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty result content")
	}
	return res.Content[0].Text
}

func TestCoreAdoptsFirstTab(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	core.mu.Lock()
	sid := core.sessionID
	core.mu.Unlock()
	if sid != "S1" {
		t.Errorf("current session = %q, want S1 (first page adopted)", sid)
	}
}

func TestTabsList(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	txt := firstText(t, callTool(t, ctx, reg, "tabs", `{"action":"list"}`))
	if !strings.Contains(txt, "t1") || !strings.Contains(txt, "t3") {
		t.Errorf("list missing pages: %s", txt)
	}
	if strings.Contains(txt, "t2") {
		t.Errorf("list must exclude the iframe (t2): %s", txt)
	}
}

func TestTabsNewSelectNav(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	// new -> returns the created target id
	txt := firstText(t, callTool(t, ctx, reg, "tabs", `{"action":"new","url":"http://x"}`))
	if !strings.Contains(txt, "new1") {
		t.Errorf("new result = %s", txt)
	}
	// nav -> ok
	txt = firstText(t, callTool(t, ctx, reg, "tabs", `{"action":"nav","url":"http://y"}`))
	if !strings.Contains(txt, "navigated") {
		t.Errorf("nav result = %s", txt)
	}
	// missing arg -> tool-level error (IsError), not a protocol error
	res := callTool(t, ctx, reg, "tabs", `{"action":"close"}`) // no targetId
	if !res.IsError {
		t.Errorf("close without targetId: want IsError")
	}
}

func TestTabsViewport(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	// current tab is t1; set a viewport override on it
	txt := firstText(t, callTool(t, ctx, reg, "tabs", `{"action":"viewport","targetId":"t1","width":375,"height":812}`))
	if !strings.Contains(txt, "set to 375x812") {
		t.Errorf("viewport set = %s", txt)
	}
	// omit size -> clear the override
	txt = firstText(t, callTool(t, ctx, reg, "tabs", `{"action":"viewport","targetId":"t1"}`))
	if !strings.Contains(txt, "cleared") {
		t.Errorf("viewport clear = %s", txt)
	}
	// viewport requires a targetId
	res := callTool(t, ctx, reg, "tabs", `{"action":"viewport","width":100,"height":100}`)
	if !res.IsError {
		t.Errorf("viewport without targetId: want IsError")
	}
}

func TestWindow(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	// info -> reads window bounds (no targetId -> current tab t1)
	txt := firstText(t, callTool(t, ctx, reg, "window", `{"action":"info"}`))
	if !strings.Contains(txt, "windowId") || !strings.Contains(txt, "1265") {
		t.Errorf("window info = %s", txt)
	}
	// set -> applies bounds then re-reads
	txt = firstText(t, callTool(t, ctx, reg, "window", `{"action":"set","width":1000,"height":700}`))
	if !strings.Contains(txt, "windowId") {
		t.Errorf("window set = %s", txt)
	}
	// set with no dims/state -> error
	res := callTool(t, ctx, reg, "window", `{"action":"set"}`)
	if !res.IsError {
		t.Errorf("window set with no bounds: want IsError")
	}
}

func TestTabsCloseCurrentReadopts(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	core.mu.Lock()
	first := core.targetID
	core.mu.Unlock()
	if first != "t1" {
		t.Fatalf("initial current = %q, want t1", first)
	}

	// Close the current tab; Core should adopt the next live page (t3).
	callTool(t, ctx, reg, "tabs", `{"action":"close","targetId":"t1"}`)
	core.mu.Lock()
	now := core.targetID
	core.mu.Unlock()
	if now != "t3" {
		t.Errorf("after closing current t1, current = %q, want t3", now)
	}
}

func TestPointerBySelector(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatal(err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	// content quad centers at (20,20)
	txt := firstText(t, callTool(t, ctx, reg, "pointer", `{"action":"click","selector":"#btn"}`))
	if !strings.Contains(txt, "click at 20,20") {
		t.Errorf("pointer click = %s", txt)
	}
	// selector that matches nothing -> tool-level error
	res := callTool(t, ctx, reg, "pointer", `{"action":"click","selector":"#missing"}`)
	if !res.IsError {
		t.Errorf("pointer on missing selector: want IsError")
	}
}

func TestPointerByAt(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatal(err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	txt := firstText(t, callTool(t, ctx, reg, "pointer", `{"action":"rightclick","at":"100,200"}`))
	if !strings.Contains(txt, "rightclick at 100,200") {
		t.Errorf("pointer at = %s", txt)
	}
}

func TestInput(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()
	core, err := NewCore(ctx, hostPort)
	if err != nil {
		t.Fatal(err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)

	txt := firstText(t, callTool(t, ctx, reg, "input", `{"selector":"#q","text":"hello"}`))
	if !strings.Contains(txt, "typed 5 chars") {
		t.Errorf("input = %s", txt)
	}
}

// newRegCore spins up a fake-CDP-backed Core + registry for a single test.
func newRegCore(t *testing.T) (*mcptransport.Registry, context.Context) {
	t.Helper()
	ctx := context.Background()
	core, err := NewCore(ctx, startFakeCDP(t))
	if err != nil {
		t.Fatal(err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)
	RegisterDev(reg, core)
	RegisterFiles(reg, core)
	return reg, ctx
}

// newRegCoreWithBox is newRegCore with a jailed file root (for saveAs/upload).
func newRegCoreWithBox(t *testing.T, dir string) (*mcptransport.Registry, context.Context) {
	t.Helper()
	ctx := context.Background()
	core, err := NewCore(ctx, startFakeCDP(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SetFileRoot(dir, "", ""); err != nil {
		t.Fatal(err)
	}
	reg := mcptransport.NewRegistry("")
	Register(reg, core)
	RegisterDev(reg, core)
	RegisterFiles(reg, core)
	return reg, ctx
}

func TestScroll(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "scroll", `{"action":"by","dy":300}`)); !strings.Contains(txt, "scrolled by") {
		t.Errorf("scroll by = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "scroll", `{"action":"top"}`)); !strings.Contains(txt, "scrolled top") {
		t.Errorf("scroll top = %s", txt)
	}
}

func TestWaitTime(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "wait", `{"action":"time","ms":1}`)); !strings.Contains(txt, "waited 1ms") {
		t.Errorf("wait time = %s", txt)
	}
}

func TestWaitSelector(t *testing.T) {
	reg, ctx := newRegCore(t)
	// #x resolves to nodeId 5 in the fake -> present
	if res := callTool(t, ctx, reg, "wait", `{"action":"selector","selector":"#x"}`); res.IsError {
		t.Errorf("wait selector present = %+v", res)
	}
	// #missing is absent; gone=true should succeed fast
	if res := callTool(t, ctx, reg, "wait", `{"action":"selector","selector":"#missing","gone":true,"timeout":1}`); res.IsError {
		t.Errorf("wait selector gone = %+v", res)
	}
	// #missing present should time out -> tool error
	if res := callTool(t, ctx, reg, "wait", `{"action":"selector","selector":"#missing","timeout":0.3}`); !res.IsError {
		t.Errorf("wait selector missing: want timeout IsError, got %+v", res)
	}
}

func TestWaitText(t *testing.T) {
	reg, ctx := newRegCore(t)
	if res := callTool(t, ctx, reg, "wait", `{"action":"text","text":"hello","timeout":1}`); res.IsError {
		t.Errorf("wait text = %+v", res)
	}
}

func TestWaitURL(t *testing.T) {
	reg, ctx := newRegCore(t)
	if res := callTool(t, ctx, reg, "wait", `{"action":"url","url":"abc","timeout":1}`); res.IsError {
		t.Errorf("wait url = %+v", res)
	}
}

func TestRead(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "read", `{"kind":"text"}`)); !strings.Contains(txt, "visible page text") {
		t.Errorf("read text = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "read", `{"kind":"html"}`)); !strings.Contains(txt, "hello world") {
		t.Errorf("read html = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "read", `{"kind":"value","selector":"#q"}`)); !strings.Contains(txt, "field-val-42") {
		t.Errorf("read value = %s", txt)
	}
	txt := firstText(t, callTool(t, ctx, reg, "read", `{"kind":"grid","selector":"table"}`))
	if !strings.Contains(txt, "headers") || !strings.Contains(txt, "Apple") {
		t.Errorf("read grid = %s", txt)
	}
}

func TestFind(t *testing.T) {
	reg, ctx := newRegCore(t)
	txt := firstText(t, callTool(t, ctx, reg, "find", `{"query":"save button"}`))
	if !strings.Contains(txt, "Save changes") || !strings.Contains(txt, "#save") {
		t.Errorf("find = %s", txt)
	}
}

func TestEvaluate(t *testing.T) {
	reg, ctx := newRegCore(t)
	txt := firstText(t, callTool(t, ctx, reg, "evaluate", `{"expression":"EVALTEST 1+1"}`))
	if txt != "42" {
		t.Errorf("evaluate = %s, want 42", txt)
	}
}

func TestConsole(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "console", `{"action":"list"}`)); txt != "(no console messages)" {
		t.Errorf("console empty = %s", txt)
	}
	if res := callTool(t, ctx, reg, "console", `{"action":"clear"}`); res.IsError {
		t.Errorf("console clear = %+v", res)
	}
}

func TestNetwork(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "network", `{"action":"list"}`)); txt != "(no network requests)" {
		t.Errorf("network empty = %s", txt)
	}
	if res := callTool(t, ctx, reg, "network", `{"action":"clear"}`); res.IsError {
		t.Errorf("network clear = %+v", res)
	}
}

func TestCapString(t *testing.T) {
	if got := capString("hello", 100); got != "hello" {
		t.Errorf("short = %q", got)
	}
	long := strings.Repeat("\u00e9", 1000) // 2 bytes each, 2000 total
	got := capString(long, 11)
	if len(got) >= len(long) {
		t.Fatalf("did not truncate")
	}
	if !strings.Contains(got, "[truncated:") || !strings.Contains(got, "2000 bytes") {
		t.Errorf("marker = %q", got)
	}
	if strings.Contains(got, "\ufffd") {
		t.Errorf("split a rune")
	}
	if got := capString("abc", 0); got != "abc" {
		t.Errorf("default cap = %q", got)
	}
}

func TestEmulate(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "emulate", `{"reset":true}`)); txt != "emulation reset" {
		t.Errorf("emulate reset = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "emulate", `{"width":400,"height":800}`)); !strings.Contains(txt, "viewport 400x800") {
		t.Errorf("emulate viewport = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "emulate", `{}`)); txt != "no emulation fields set" {
		t.Errorf("emulate empty = %s", txt)
	}
}

func TestStorage(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "storage", `{"type":"cookie","action":"get"}`)); txt != "(no cookies)" {
		t.Errorf("storage cookie get = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "storage", `{"type":"cookie","action":"set","name":"a","value":"b"}`)); txt != "cookie set: a" {
		t.Errorf("storage cookie set = %s", txt)
	}
}

func TestCDPCall(t *testing.T) {
	reg, ctx := newRegCore(t)
	if txt := firstText(t, callTool(t, ctx, reg, "cdp_call", `{"method":"DOM.getDocument"}`)); !strings.Contains(txt, "nodeId") {
		t.Errorf("cdp_call = %s", txt)
	}
	if res := callTool(t, ctx, reg, "cdp_call", `{}`); !res.IsError {
		t.Errorf("cdp_call without method should error")
	}
}

func TestReadScreenshot(t *testing.T) {
	reg, ctx := newRegCore(t)
	// inline image result (default png)
	res := callTool(t, ctx, reg, "read", `{"kind":"screenshot"}`)
	if len(res.Content) == 0 || res.Content[0].Type != "image" {
		t.Fatalf("screenshot not image: %+v", res.Content)
	}
	if res.Content[0].Data == "" || res.Content[0].MimeType != "image/png" {
		t.Errorf("screenshot part = %+v", res.Content[0])
	}
	// jpeg format + mime
	res = callTool(t, ctx, reg, "read", `{"kind":"screenshot","format":"jpeg","quality":50}`)
	if res.Content[0].MimeType != "image/jpeg" {
		t.Errorf("jpeg mime = %s", res.Content[0].MimeType)
	}
	// saveAs without a configured root -> refused (jail not enabled)
	res = callTool(t, ctx, reg, "read", `{"kind":"screenshot","saveAs":"shot.png"}`)
	if txt := firstText(t, res); !strings.Contains(txt, "no file root") {
		t.Errorf("saveAs without root = %s", txt)
	}
	// with a jailed root, saveAs writes relative to it
	dir := t.TempDir()
	reg2, ctx2 := newRegCoreWithBox(t, dir)
	res = callTool(t, ctx2, reg2, "read", `{"kind":"screenshot","saveAs":"shot.png"}`)
	if txt := firstText(t, res); !strings.Contains(txt, "screenshot saved") {
		t.Errorf("saveAs = %s", txt)
	}
	if b, err := os.ReadFile(dir + "/shot.png"); err != nil || len(b) == 0 {
		t.Errorf("screenshot file not written: err=%v len=%d", err, len(b))
	}
	// parent-traversal path is confined (rejected)
	res = callTool(t, ctx2, reg2, "read", `{"kind":"screenshot","saveAs":"../evil.png"}`)
	if !res.IsError {
		t.Errorf("saveAs traversal should be rejected")
	}
}

func TestUpload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/up.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, ctx := newRegCoreWithBox(t, dir)
	res := callTool(t, ctx, reg, "upload", `{"selector":"input[type=file]","files":["/up.txt"]}`)
	if txt := firstText(t, res); !strings.Contains(txt, "uploaded 1") || !strings.Contains(txt, "/up.txt") {
		t.Errorf("upload = %s", txt)
	}
	// missing file -> error, and no host path leaks
	res = callTool(t, ctx, reg, "upload", `{"selector":"input","files":["/nope.txt"]}`)
	if !res.IsError {
		t.Errorf("upload missing file should error")
	} else if strings.Contains(firstText(t, res), dir) {
		t.Errorf("upload error leaked host path: %s", firstText(t, res))
	}
}

func TestDownload(t *testing.T) {
	dir := t.TempDir()
	reg, ctx := newRegCoreWithBox(t, dir)
	if txt := firstText(t, callTool(t, ctx, reg, "download", `{"action":"list"}`)); !strings.Contains(txt, "no downloads") {
		t.Errorf("list = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "download", `{"action":"start"}`)); !strings.Contains(txt, "capture started") {
		t.Errorf("start = %s", txt)
	}
	if txt := firstText(t, callTool(t, ctx, reg, "download", `{"action":"clear"}`)); !strings.Contains(txt, "cleared") {
		t.Errorf("clear = %s", txt)
	}
	// start without a file box -> ErrNoRoot
	reg2, ctx2 := newRegCore(t)
	if res := callTool(t, ctx2, reg2, "download", `{"action":"start"}`); !res.IsError {
		t.Errorf("download start without box should error")
	}
}
