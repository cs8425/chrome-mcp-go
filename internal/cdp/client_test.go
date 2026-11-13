package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startFakeCDP stands up a minimal CDP endpoint over WebSocket so the client's
// JSON-RPC framing, response routing, event dispatch and error path are tested
// without a real Chrome. It answers a fixed set of methods and, on
// Page.enable, pushes a Page.javascriptDialogOpening event before responding.
func startFakeCDP(t *testing.T) (hostPort string) {
	t.Helper()
	mux := http.NewServeMux()
	var wsURL string

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
			// Browser->client push, ahead of the response.
			if req.Method == "Page.enable" {
				ev, _ := json.Marshal(map[string]any{
					"method":    "Page.javascriptDialogOpening",
					"sessionId": req.SessionID,
					"params":    map[string]any{"message": "hi", "type": "alert"},
				})
				_ = c.Write(ctx, websocket.MessageText, ev)
			}

			out := map[string]any{"id": req.ID}
			switch req.Method {
			case "Target.getTargets":
				out["result"] = map[string]any{"targetInfos": []map[string]any{
					{"targetId": "t1", "type": "page", "title": "A", "url": "http://a"},
					{"targetId": "t2", "type": "iframe"},
					{"targetId": "t3", "type": "page", "title": "C", "url": "http://c"},
				}}
			case "Page.navigate":
				out["result"] = map[string]any{"frameId": "F1"}
			case "Target.attachToTarget":
				out["result"] = map[string]any{"sessionId": "S1"}
			case "Target.createTarget":
				out["result"] = map[string]any{"targetId": "new1"}
			case "Page.enable", "Page.disable", "Target.activateTarget",
				"Target.closeTarget", "Emulation.setDeviceMetricsOverride",
				"Page.handleJavaScriptDialog":
				out["result"] = map[string]any{}
			default:
				out["error"] = map[string]any{"code": -32601, "message": "not found: " + req.Method}
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

func TestDialBrowserAndCall(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()

	conn, err := DialBrowser(ctx, hostPort)
	if err != nil {
		t.Fatalf("DialBrowser: %v", err)
	}
	defer conn.Close()

	var g struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			Title    string `json:"title"`
		} `json:"targetInfos"`
	}
	if err := conn.Client("").RawCall(ctx, "Target.getTargets", nil, &g); err != nil {
		t.Fatalf("Target.getTargets: %v", err)
	}
	if len(g.TargetInfos) != 3 {
		t.Fatalf("targetInfos = %d, want 3", len(g.TargetInfos))
	}
	if g.TargetInfos[0].Title != "A" {
		t.Errorf("first target title = %q, want A", g.TargetInfos[0].Title)
	}
}

func TestEventDispatch(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()

	conn, err := DialBrowser(ctx, hostPort)
	if err != nil {
		t.Fatalf("DialBrowser: %v", err)
	}
	defer conn.Close()

	sess := conn.Client("S1")
	got := make(chan json.RawMessage, 1)
	unsub := sess.Subscribe("Page.javascriptDialogOpening", func(p json.RawMessage) {
		got <- p
	})
	defer unsub()

	// The fake pushes the event when it receives Page.enable on this session.
	if err := sess.RawCall(ctx, "Page.enable", nil, nil); err != nil {
		t.Fatalf("Page.enable: %v", err)
	}
	select {
	case p := <-got:
		var d struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		_ = json.Unmarshal(p, &d)
		if d.Message != "hi" || d.Type != "alert" {
			t.Errorf("event = %+v, want {hi alert}", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}
}

func TestRPCError(t *testing.T) {
	hostPort := startFakeCDP(t)
	ctx := context.Background()

	conn, err := DialBrowser(ctx, hostPort)
	if err != nil {
		t.Fatalf("DialBrowser: %v", err)
	}
	defer conn.Close()

	var out map[string]any
	err = conn.Client("").RawCall(ctx, "Nope.method", nil, &out)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var re *rpcError
	if !errors.As(err, &re) || re.Code != -32601 {
		t.Errorf("err = %v, want rpcError code -32601", err)
	}
}
