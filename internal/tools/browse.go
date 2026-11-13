package tools

// The browse pack: interaction tools built on DOM resolution + the Input
// domain. P2a ships the element resolver plus pointer and input.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"chrome-mcp/internal/cdp"
	"mcptransport"
)

// resolveNode resolves a CSS selector to a DOM node id in the current tab and
// scrolls it into view, so a subsequent getBoxModel is within the viewport.
func (c *Core) resolveNode(ctx context.Context, selector string) (*cdp.Client, int64, error) {
	page := c.page()
	if err := page.RawCall(ctx, "DOM.enable", nil, nil); err != nil {
		return nil, 0, err
	}
	var doc struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	if err := page.RawCall(ctx, "DOM.getDocument", map[string]any{"depth": 1}, &doc); err != nil {
		return nil, 0, err
	}
	var qs struct {
		NodeID int64 `json:"nodeId"`
	}
	if err := page.RawCall(ctx, "DOM.querySelector",
		map[string]any{"nodeId": doc.Root.NodeID, "selector": selector}, &qs); err != nil {
		return nil, 0, err
	}
	if qs.NodeID == 0 {
		return nil, 0, fmt.Errorf("no element matches %q", selector)
	}
	// Best effort: a missing scroll should not block the box query.
	_ = page.RawCall(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"nodeId": qs.NodeID}, nil)
	return page, qs.NodeID, nil
}

// nodeCenter returns the viewport center of a node's content box (CSS px,
// the same space Input.dispatchMouseEvent uses). DOM.getBoxModel wraps the
// quads under a top-level "model" field.
func nodeCenter(ctx context.Context, page *cdp.Client, id int64) (float64, float64, error) {
	var res struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := page.RawCall(ctx, "DOM.getBoxModel", map[string]any{"nodeId": id}, &res); err != nil {
		return 0, 0, err
	}
	if len(res.Model.Content) < 8 {
		return 0, 0, fmt.Errorf("unexpected box model for node %d", id)
	}
	var sx, sy float64
	for i := 0; i < 8; i += 2 {
		sx += res.Model.Content[i]
		sy += res.Model.Content[i+1]
	}
	return sx / 4, sy / 4, nil
}

// mouse dispatches one Input domain mouse event.
func mouse(ctx context.Context, page *cdp.Client, typ string, x, y float64, button string, clickCount int) error {
	p := map[string]any{"type": typ, "x": x, "y": y}
	if button != "" {
		p["button"] = button
	}
	if clickCount > 0 {
		p["clickCount"] = clickCount
	}
	return page.RawCall(ctx, "Input.dispatchMouseEvent", p, nil)
}

// ---------- pointer ----------

var pointerTool = &mcptransport.Tool{
	Name:        "pointer",
	Description: "Mouse actions on the current tab: click, hover, dblclick, rightclick. Address an element by CSS selector or an explicit x,y viewport point.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action": {
				Type:        mcptransport.TypeString,
				Description: "click | hover | dblclick | rightclick",
				Enum:        []string{"click", "hover", "dblclick", "rightclick"},
			},
			"selector": {Type: mcptransport.TypeString, Description: "CSS selector of the target element"},
			"at":       {Type: mcptransport.TypeString, Description: `explicit viewport point "x,y" (overrides selector)`},
		},
		Required: []string{"action"},
	},
}

func (c *Core) pointer(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action   string `json:"action"`
		Selector string `json:"selector"`
		At       string `json:"at"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}

	page := c.page()
	var x, y float64
	if a.At != "" {
		parts := strings.Split(a.At, ",")
		if len(parts) != 2 {
			return mcptransport.ResultError(`at must be "x,y"`), nil
		}
		var err error
		if x, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err != nil {
			return mcptransport.ResultError("bad at x: " + err.Error()), nil
		}
		if y, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err != nil {
			return mcptransport.ResultError("bad at y: " + err.Error()), nil
		}
	} else {
		if a.Selector == "" {
			return mcptransport.ResultError("pointer needs selector or at"), nil
		}
		_, id, err := c.resolveNode(ctx, a.Selector)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if x, y, err = nodeCenter(ctx, page, id); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
	}

	switch a.Action {
	case "click":
		_ = mouse(ctx, page, "mouseMoved", x, y, "", 0)
		_ = mouse(ctx, page, "mousePressed", x, y, "left", 1)
		_ = mouse(ctx, page, "mouseReleased", x, y, "left", 1)
	case "dblclick":
		_ = mouse(ctx, page, "mouseMoved", x, y, "", 0)
		_ = mouse(ctx, page, "mousePressed", x, y, "left", 1)
		_ = mouse(ctx, page, "mouseReleased", x, y, "left", 1)
		_ = mouse(ctx, page, "mousePressed", x, y, "left", 2)
		_ = mouse(ctx, page, "mouseReleased", x, y, "left", 2)
	case "rightclick":
		_ = mouse(ctx, page, "mouseMoved", x, y, "", 0)
		_ = mouse(ctx, page, "mousePressed", x, y, "right", 1)
		_ = mouse(ctx, page, "mouseReleased", x, y, "right", 1)
	case "hover":
		_ = mouse(ctx, page, "mouseMoved", x, y, "", 0)
	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
	return mcptransport.ResultText(fmt.Sprintf("%s at %d,%d", a.Action, int(x), int(y))), nil
}

// ---------- input ----------

var inputTool = &mcptransport.Tool{
	Name:        "input",
	Description: "Focus an element on the current tab and type text into it (real input events via Input.insertText).",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"selector": {Type: mcptransport.TypeString, Description: "CSS selector of the target element"},
			"text":     {Type: mcptransport.TypeString, Description: "text to type (omit to just focus)"},
		},
		Required: []string{"selector"},
	},
}

func (c *Core) input(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Selector string `json:"selector"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	if a.Selector == "" {
		return mcptransport.ResultError("input needs selector"), nil
	}
	page, id, err := c.resolveNode(ctx, a.Selector)
	if err != nil {
		return mcptransport.ResultError(err.Error()), nil
	}
	if err := page.RawCall(ctx, "DOM.focus", map[string]any{"nodeId": id}, nil); err != nil {
		return mcptransport.ResultError("focus: " + err.Error()), nil
	}
	if a.Text != "" {
		if err := page.RawCall(ctx, "Input.insertText", map[string]any{"text": a.Text}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
	}
	return mcptransport.ResultText(fmt.Sprintf("typed %d chars into %s", len(a.Text), a.Selector)), nil
}
