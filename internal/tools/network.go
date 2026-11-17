package tools

// The dev pack (cont.): network. HTTP requests for a tab are captured from the
// Network domain events and buffered per target; the `network` tool reads them.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"chrome-mcp/internal/cdp"
	"mcptransport"
)

// netRec is one captured request/response.
type netRec struct {
	ID       string `json:"-"`
	Method   string `json:"method"`
	URL      string `json:"url"`
	Type     string `json:"type,omitempty"`
	Status   int    `json:"status,omitempty"`
	MIME     string `json:"mime,omitempty"`
	Failed   bool   `json:"failed,omitempty"`
	ErrText  string `json:"error,omitempty"`
	Canceled bool   `json:"canceled,omitempty"`
}

const maxNetRecords = 300

// startNetworkCapture subscribes a tab's session to Network events, buffering
// records under targetID. Per-target and persistent like console capture: a tab
// keeps being captured even when not current; re-selecting is a no-op.
func (c *Core) startNetworkCapture(ctx context.Context, page *cdp.Client, targetID string) {
	if !c.claimNetwork(targetID) {
		return
	}
	var unsubs []func()

	unsubs = append(unsubs, page.Subscribe("Network.requestWillBeSent", func(params json.RawMessage) {
		var e struct {
			RequestID string `json:"requestId"`
			Type      string `json:"type"`
			Request   struct {
				Method string `json:"method"`
				URL    string `json:"url"`
			} `json:"request"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		c.networksMu.Lock()
		c.networks[targetID] = append(c.networks[targetID],
			&netRec{ID: e.RequestID, Method: e.Request.Method, URL: e.Request.URL, Type: e.Type})
		if n := len(c.networks[targetID]); n > maxNetRecords {
			c.networks[targetID] = c.networks[targetID][n-maxNetRecords:]
		}
		c.networksMu.Unlock()
	}))

	unsubs = append(unsubs, page.Subscribe("Network.responseReceived", func(params json.RawMessage) {
		var e struct {
			RequestID string `json:"requestId"`
			Response  struct {
				Status   int    `json:"status"`
				MimeType string `json:"mimeType"`
			} `json:"response"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		c.networksMu.Lock()
		if r := c.findNet(targetID, e.RequestID); r != nil {
			r.Status = e.Response.Status
			r.MIME = e.Response.MimeType
		}
		c.networksMu.Unlock()
	}))

	unsubs = append(unsubs, page.Subscribe("Network.loadingFailed", func(params json.RawMessage) {
		var e struct {
			RequestID string `json:"requestId"`
			ErrorText string `json:"errorText"`
			Canceled  bool   `json:"canceled"`
		}
		if json.Unmarshal(params, &e) != nil {
			return
		}
		c.networksMu.Lock()
		if r := c.findNet(targetID, e.RequestID); r != nil {
			r.Failed = true
			r.ErrText = e.ErrorText
			r.Canceled = e.Canceled
		}
		c.networksMu.Unlock()
	}))

	c.mu.Lock()
	c.networkSubs[targetID] = unsubs
	c.mu.Unlock()

	_ = page.RawCall(ctx, "Network.enable", nil, nil)
}

// claimNetwork reserves targetID for network capture (see claimConsole).
func (c *Core) claimNetwork(targetID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.networkSubs == nil {
		c.networkSubs = map[string][]func(){}
	}
	if _, ok := c.networkSubs[targetID]; ok {
		return false
	}
	c.networkSubs[targetID] = []func(){} // reserved; replaced with real subs below
	return true
}

// findNet returns the record with the given request id (most recent match).
// The caller must hold networksMu.
func (c *Core) findNet(targetID, id string) *netRec {
	recs := c.networks[targetID]
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].ID == id {
			return recs[i]
		}
	}
	return nil
}

func matchStatus(r *netRec, want string) bool {
	if want == "" {
		return true
	}
	switch want {
	case "failed":
		return r.Failed
	case "2xx":
		return r.Status >= 200 && r.Status < 300
	case "3xx":
		return r.Status >= 300 && r.Status < 400
	case "4xx":
		return r.Status >= 400 && r.Status < 500
	case "5xx":
		return r.Status >= 500 && r.Status < 600
	default:
		var code int
		fmt.Sscanf(want, "%d", &code)
		return code != 0 && r.Status == code
	}
}

var networkTool = &mcptransport.Tool{
	Name:        "network",
	Description: "Read captured HTTP requests for a tab (default the current tab; pass targetId to read another without switching). Every tab is captured continuously (from first selection/list/creation), so switching tabs never loses events. Filter by url (substring), method, status (2xx|3xx|4xx|5xx|failed|exact code); limit to the most recent N; clear empties the buffer.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action":   {Type: mcptransport.TypeString, Description: "list (default) | clear", Enum: []string{"list", "clear"}},
			"targetId": {Type: mcptransport.TypeString, Description: "read (and start capturing) this specific tab by id; defaults to the current tab — inspect several tabs without switching current"},
			"url":      {Type: mcptransport.TypeString, Description: "only requests whose URL contains this substring"},
			"method":   {Type: mcptransport.TypeString, Description: "only this HTTP method"},
			"status":   {Type: mcptransport.TypeString, Description: "2xx|3xx|4xx|5xx|failed or an exact code like 404"},
			"limit":    {Type: mcptransport.TypeNumber, Description: "most recent N matching requests"},
		},
	},
}

func (c *Core) network(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action   string `json:"action"`
		TargetID string `json:"targetId"`
		URL      string `json:"url"`
		Method   string `json:"method"`
		Status   string `json:"status"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	// Read a specific tab by id (default: current). Naming a tab also starts its
	// capture (idempotent, without making it current) so multi-tab reads need no
	// current-tab switching — and no select/read race.
	target := a.TargetID
	if target == "" {
		target = c.currentTargetID()
	} else {
		c.captureTarget(ctx, target)
	}

	if a.Action == "clear" {
		c.networksMu.Lock()
		delete(c.networks, target)
		c.networksMu.Unlock()
		return mcptransport.ResultText("network cleared"), nil
	}

	c.networksMu.Lock()
	recs := append([]*netRec{}, c.networks[target]...)
	c.networksMu.Unlock()

	var out []*netRec
	for _, r := range recs {
		if a.URL != "" && !strings.Contains(r.URL, a.URL) {
			continue
		}
		if a.Method != "" && !strings.EqualFold(r.Method, a.Method) {
			continue
		}
		if !matchStatus(r, a.Status) {
			continue
		}
		out = append(out, r)
	}
	if a.Limit > 0 && len(out) > a.Limit {
		out = out[len(out)-a.Limit:]
	}
	if len(out) == 0 {
		return mcptransport.ResultText("(no network requests)"), nil
	}
	var b strings.Builder
	b.WriteString("[")
	for i, r := range out {
		if i > 0 {
			b.WriteString(",\n")
		}
		j, _ := json.Marshal(r)
		b.Write(j)
	}
	b.WriteString("]")
	return mcptransport.ResultText(c.cap(b.String(), 0)), nil
}
