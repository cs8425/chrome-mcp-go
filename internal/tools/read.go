package tools

// The browse pack (cont.): read. Text/value/grid use Runtime.evaluate with
// returnByValue for accurate DOM reads (the standalone `evaluate` tool is a
// separate, dev-pack-gated surface added in P3).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"chrome-mcp/internal/cdp"
	"chrome-mcp/internal/filebox"
	"mcptransport"
)

// evalValue runs Runtime.evaluate with returnByValue and returns the JSON
// value the expression produced.
func evalValue(ctx context.Context, page *cdp.Client, expr string) (json.RawMessage, error) {
	var res struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := page.RawCall(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": false,
	}, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, fmt.Errorf("eval error: %s", res.ExceptionDetails.Text)
	}
	return res.Result.Value, nil
}

// jsString returns s as a JS string literal (safely quoted+escaped).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var readTool = &mcptransport.Tool{
	Name:        "read",
	Description: "Read content from the page: kind text (visible text), html (markup), value (a form field's current value), grid (a table as {headers, rows}), or screenshot (PNG/JPEG/WebP image returned inline to the model; use saveAs to write a file instead). Screenshot options: format/quality, fullPage, clip (a \"x,y,width,height\" region in CSS px, page coordinates), and scale (0.1-3, renderer-side so text stays crisp; with no clip it scales the current viewport). When saved, the result reports mode, resolved clip/scale, output size, format and byte count.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"kind": {
				Type:        mcptransport.TypeString,
				Description: "text | html | value | grid | screenshot (default text)",
				Enum:        []string{"text", "html", "value", "grid", "screenshot"},
			},
			"selector": {Type: mcptransport.TypeString, Description: "value: field selector; grid: table selector (default: first table)"},
			"maxBytes": {Type: mcptransport.TypeNumber, Description: "truncate result to this many bytes (default 50000)"},
			"format":   {Type: mcptransport.TypeString, Description: "screenshot: png (default) | jpeg | webp", Enum: []string{"png", "jpeg", "webp"}},
			"quality":  {Type: mcptransport.TypeNumber, Description: "screenshot: compression quality 0-100 (jpeg/webp only)"},
			"fullPage": {Type: mcptransport.TypeBoolean, Description: "screenshot: capture beyond the viewport (full scrollable page)"},
			"clip":     {Type: mcptransport.TypeString, Description: `screenshot: region to capture, "x,y,width,height" in CSS px (page coordinates)`},
			"scale":    {Type: mcptransport.TypeNumber, Description: "screenshot: output scale 0.1-3 (renderer-side, text stays crisp); with no clip it scales the current viewport"},
			"saveAs":   {Type: mcptransport.TypeString, Description: "screenshot: write the image to this file path and return the path instead of the inline image"},
		},
	},
}

func (c *Core) read(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Kind     string  `json:"kind"`
		Selector string  `json:"selector"`
		MaxBytes int     `json:"maxBytes"`
		Format   string  `json:"format"`
		Quality  int     `json:"quality"`
		FullPage bool    `json:"fullPage"`
		Clip     string  `json:"clip"`
		Scale    float64 `json:"scale"`
		SaveAs   string  `json:"saveAs"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	kind := a.Kind
	if kind == "" {
		kind = "text"
	}
	page := c.page()

	switch kind {
	case "html":
		var doc struct {
			Root struct {
				NodeID int64 `json:"nodeId"`
			} `json:"root"`
		}
		if err := page.RawCall(ctx, "DOM.getDocument", map[string]any{"depth": 1}, &doc); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		var oh struct {
			OuterHTML string `json:"outerHTML"`
		}
		if err := page.RawCall(ctx, "DOM.getOuterHTML", map[string]any{"nodeId": doc.Root.NodeID}, &oh); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		return mcptransport.ResultText(c.cap(oh.OuterHTML, a.MaxBytes)), nil

	case "text":
		val, err := evalValue(ctx, page, `document.body ? document.body.innerText : ""`)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		var s string
		if err := json.Unmarshal(val, &s); err != nil {
			s = string(val)
		}
		return mcptransport.ResultText(c.cap(s, a.MaxBytes)), nil

	case "value":
		if a.Selector == "" {
			return mcptransport.ResultError("read value needs selector"), nil
		}
		expr := `(function(){var el=document.querySelector(` + jsString(a.Selector) + `);if(!el)return null;return (el.value!==undefined)?String(el.value):el.textContent;})()`
		val, err := evalValue(ctx, page, expr)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if string(val) == "null" {
			return mcptransport.ResultError("no element matches " + a.Selector), nil
		}
		var v any
		_ = json.Unmarshal(val, &v)
		if s, ok := v.(string); ok {
			return mcptransport.ResultText(c.cap(s, a.MaxBytes)), nil
		}
		return mcptransport.ResultText(c.cap(fmt.Sprint(v), a.MaxBytes)), nil

	case "grid":
		sel := "null"
		if a.Selector != "" {
			sel = jsString(a.Selector)
		}
		expr := `(function(){var s=` + sel + `;var t=s?document.querySelector(s):document.querySelector('table');if(!t)return null;var rows=t.rows||[];var hs=[],bs=[];for(var i=0;i<rows.length;i++){var cs=rows[i].cells||[];var cells=[];for(var j=0;j<cs.length;j++)cells.push(cs[j].textContent.trim());if(i===0)hs=cells;else bs.push(cells);}return {headers:hs,rows:bs};})()`
		val, err := evalValue(ctx, page, expr)
		if err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if string(val) == "null" {
			return mcptransport.ResultError("no table found"), nil
		}
		return mcptransport.ResultText(c.cap(string(normalizeJSON(val)), a.MaxBytes)), nil

	case "screenshot":
		format := a.Format
		if format == "" {
			format = "png"
		}
		params := map[string]any{"format": format}
		if (format == "jpeg" || format == "webp") && a.Quality > 0 {
			params["quality"] = a.Quality
		}
		mode := "viewport"
		clip := map[string]any{}
		hasClip := false
		if a.Clip != "" {
			x, y, w, h, ok := parseClip(a.Clip)
			if !ok {
				return mcptransport.ResultError("bad clip: " + a.Clip + ` (want "x,y,width,height" in CSS px)`), nil
			}
			clip["x"], clip["y"], clip["width"], clip["height"] = x, y, w, h
			hasClip = true
			mode = "region"
		}
		// A scale with no explicit region is applied over the current viewport box.
		if !hasClip && a.Scale > 0 {
			vpBox, verr := viewportBox(ctx, page)
			if verr != nil {
				return mcptransport.ResultError(verr.Error()), nil
			}
			clip = vpBox
			hasClip = true
		}
		scale := 1.0
		if a.Scale > 0 {
			scale = clampScale(a.Scale)
		}
		if hasClip {
			clip["scale"] = scale
			params["clip"] = clip
		}
		if a.FullPage && !hasClip {
			params["captureBeyondViewport"] = true
			mode = "full_page"
		}
		var shot struct {
			Data string `json:"data"`
		}
		if err := page.RawCall(ctx, "Page.captureScreenshot", params, &shot); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if shot.Data == "" {
			return mcptransport.ResultError("screenshot returned no data"), nil
		}
		raw, err := base64.StdEncoding.DecodeString(shot.Data)
		if err != nil {
			return mcptransport.ResultError("screenshot decode: " + err.Error()), nil
		}
		meta := screenshotMeta(mode, a.Clip, scale, hasClip, format, len(raw), raw)
		if a.SaveAs != "" {
			box := c.fileBox()
			if box == nil {
				return mcptransport.ResultError(filebox.ErrNoRoot.Error()), nil
			}
			if err := box.Write(ctx, a.SaveAs, raw); err != nil {
				return mcptransport.ResultError("screenshot write: " + err.Error()), nil
			}
			vp := a.SaveAs
			if cv, cerr := filebox.Canonical(a.SaveAs); cerr == nil {
				vp = cv
			}
			return mcptransport.ResultText("screenshot saved: " + vp + " (" + meta + ")"), nil
		}
		return mcptransport.ResultImage(shot.Data, "image/"+format), nil

	default:
		return mcptransport.ResultError("unknown kind: " + kind), nil
	}
}
