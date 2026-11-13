package tools

// The browse pack (cont.): scroll.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"chrome-mcp/internal/cdp"
	"mcptransport"
)

// parsePoint parses an "x,y" viewport coordinate string.
func parsePoint(s string) (float64, float64, bool) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	x, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, false
	}
	y, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, false
	}
	return x, y, true
}

// wheel dispatches a mouse-wheel scroll at (x, y).
func wheel(ctx context.Context, page *cdp.Client, x, y, dx, dy float64) error {
	return page.RawCall(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy,
	}, nil)
}

// viewportCenter returns the CSS visual viewport center via Page.getLayoutMetrics.
func viewportCenter(ctx context.Context, page *cdp.Client) (float64, float64, error) {
	var lm struct {
		CSSVisualViewport struct {
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"cssVisualViewport"`
	}
	if err := page.RawCall(ctx, "Page.getLayoutMetrics", nil, &lm); err != nil {
		return 0, 0, err
	}
	if lm.CSSVisualViewport.Width <= 0 || lm.CSSVisualViewport.Height <= 0 {
		return 640, 360, nil // sane fallback center
	}
	return lm.CSSVisualViewport.Width / 2, lm.CSSVisualViewport.Height / 2, nil
}

var scrollTool = &mcptransport.Tool{
	Name:        "scroll",
	Description: "Scroll the current tab: by a delta (mouse wheel), to an element, or to top/bottom.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"action": {
				Type:        mcptransport.TypeString,
				Description: "by | to | top | bottom",
				Enum:        []string{"by", "to", "top", "bottom"},
			},
			"dx":       {Type: mcptransport.TypeNumber, Description: "by: horizontal delta in px"},
			"dy":       {Type: mcptransport.TypeNumber, Description: "by: vertical delta in px, positive scrolls down"},
			"selector": {Type: mcptransport.TypeString, Description: "to: scroll this element into view"},
			"at":       {Type: mcptransport.TypeString, Description: `wheel origin "x,y" (default: viewport center)`},
		},
		Required: []string{"action"},
	},
}

func (c *Core) scroll(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Action   string  `json:"action"`
		DX       float64 `json:"dx"`
		DY       float64 `json:"dy"`
		Selector string  `json:"selector"`
		At       string  `json:"at"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	page := c.page()

	switch a.Action {
	case "to":
		if a.Selector == "" {
			return mcptransport.ResultError("scroll to needs selector"), nil
		}
		// resolveNode already scrolls the node into view.
		if _, _, err := c.resolveNode(ctx, a.Selector); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText("scrolled to " + a.Selector), nil

	case "by", "top", "bottom":
		x, y, err := viewportCenter(ctx, page)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if a.At != "" {
			px, py, ok := parsePoint(a.At)
			if !ok {
				return mcptransport.ResultError(`at must be "x,y"`), nil
			}
			x, y = px, py
		}
		dx, dy := a.DX, a.DY
		switch a.Action {
		case "top":
			dx, dy = 0, -1_000_000
		case "bottom":
			dx, dy = 0, 1_000_000
		}
		if err := wheel(ctx, page, x, y, dx, dy); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText(fmt.Sprintf("scrolled %s dx=%g dy=%g", a.Action, dx, dy)), nil

	default:
		return mcptransport.ResultError("unknown action: " + a.Action), nil
	}
}
