package tools

// The browse pack (cont.): wait for a condition.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"mcptransport"
)

// currentTargetID returns the current tab's target id under lock.
func (c *Core) currentTargetID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targetID
}

type waitArgs struct {
	Action   string  `json:"action"`
	MS       float64 `json:"ms"`
	Selector string  `json:"selector"`
	Gone     bool    `json:"gone"`
	Text     string  `json:"text"`
	URL      string  `json:"url"`
	Timeout  float64 `json:"timeout"`
}

var waitTool = &mcptransport.Tool{
	Name:        "wait",
	Description: "Wait for a condition on the current tab: a fixed time, a selector to appear (or disappear with gone), text to appear, or the URL to contain a substring. Returns a tool error if the deadline passes first.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action": {
				Type:        mcptransport.TypeString,
				Description: "time | selector | text | url",
				Enum:        []string{"time", "selector", "text", "url"},
			},
			"ms":       {Type: mcptransport.TypeNumber, Description: "time: milliseconds to wait"},
			"selector": {Type: mcptransport.TypeString, Description: "selector: CSS selector to wait for"},
			"gone":     {Type: mcptransport.TypeBoolean, Description: "selector/text: wait for it to be ABSENT instead of present"},
			"text":     {Type: mcptransport.TypeString, Description: "text: substring to wait for in the page's outer HTML"},
			"url":      {Type: mcptransport.TypeString, Description: "url: substring the current tab URL must contain"},
			"timeout":  {Type: mcptransport.TypeNumber, Description: "deadline in seconds (default 8)"},
		},
		Required: []string{"action"},
	},
}

func (c *Core) wait(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a waitArgs
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}

	// Fixed sleep: no CDP round-trips.
	if a.Action == "time" {
		d := time.Duration(a.MS * float64(time.Millisecond))
		if d < 0 {
			return mcptransport.ResultError("ms must be >= 0"), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d):
		}
		return mcptransport.ResultText(fmt.Sprintf("waited %s", d)), nil
	}

	if a.Timeout <= 0 {
		a.Timeout = 8
	}
	deadline := time.Now().Add(time.Duration(a.Timeout * float64(time.Second)))
	const interval = 250 * time.Millisecond

	for {
		ok, err := c.waitCondition(ctx, &a)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if ok {
			return mcptransport.ResultText("wait condition met: " + a.Action), nil
		}
		if !time.Now().Before(deadline) {
			return mcptransport.ResultError(fmt.Sprintf("timeout after %gs waiting for %s", a.Timeout, waitDesc(&a))), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func waitDesc(a *waitArgs) string {
	switch a.Action {
	case "selector":
		if a.Gone {
			return "selector " + a.Selector + " to disappear"
		}
		return "selector " + a.Selector
	case "text":
		if a.Gone {
			return "text " + a.Text + " to disappear"
		}
		return "text " + a.Text
	case "url":
		return "url containing " + a.URL
	default:
		return a.Action
	}
}

func (c *Core) waitCondition(ctx context.Context, a *waitArgs) (bool, error) {
	switch a.Action {
	case "selector":
		found, err := c.hasSelector(ctx, a.Selector)
		if err != nil {
			return false, err
		}
		if a.Gone {
			return !found, nil
		}
		return found, nil
	case "text":
		html, err := c.pageOuterHTML(ctx)
		if err != nil {
			return false, err
		}
		has := strings.Contains(html, a.Text)
		if a.Gone {
			return !has, nil
		}
		return has, nil
	case "url":
		url, err := c.currentURL(ctx)
		if err != nil {
			return false, err
		}
		return strings.Contains(url, a.URL), nil
	default:
		return false, fmt.Errorf("unknown wait action: %s", a.Action)
	}
}

func (c *Core) hasSelector(ctx context.Context, sel string) (bool, error) {
	if sel == "" {
		return false, fmt.Errorf("wait selector needs selector")
	}
	page := c.page()
	var doc struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	if err := page.RawCall(ctx, "DOM.getDocument", map[string]any{"depth": 1}, &doc); err != nil {
		return false, err
	}
	var qs struct {
		NodeID int64 `json:"nodeId"`
	}
	if err := page.RawCall(ctx, "DOM.querySelector",
		map[string]any{"nodeId": doc.Root.NodeID, "selector": sel}, &qs); err != nil {
		return false, err
	}
	return qs.NodeID != 0, nil
}

func (c *Core) pageOuterHTML(ctx context.Context) (string, error) {
	page := c.page()
	var doc struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	if err := page.RawCall(ctx, "DOM.getDocument", map[string]any{"depth": 1}, &doc); err != nil {
		return "", err
	}
	var res struct {
		OuterHTML string `json:"outerHTML"`
	}
	if err := page.RawCall(ctx, "DOM.getOuterHTML", map[string]any{"nodeId": doc.Root.NodeID}, &res); err != nil {
		return "", err
	}
	return res.OuterHTML, nil
}

func (c *Core) currentURL(ctx context.Context) (string, error) {
	var res struct {
		TargetInfo struct {
			URL string `json:"url"`
		} `json:"targetInfo"`
	}
	if err := c.browser.Load().RawCall(ctx, "Target.getTargetInfo",
		map[string]any{"targetId": c.currentTargetID()}, &res); err != nil {
		return "", err
	}
	return res.TargetInfo.URL, nil
}
