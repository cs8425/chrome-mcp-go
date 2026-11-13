package tools

// The dev pack (cont.): emulate. Device metrics, user agent, geolocation and
// prefers-color-scheme overrides via the Emulation domain. Only the fields you
// pass are applied; reset=true clears all overrides.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mcptransport"
)

var emulateTool = &mcptransport.Tool{
	Name:        "emulate",
	Description: "Emulate device/UA/geolocation/color-scheme on the current tab. Set only the fields you pass; pass reset=true to clear all overrides. width+height set the viewport (with deviceScaleFactor, mobile). colorScheme is light|dark|no-preference.",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"width":             {Type: mcptransport.TypeNumber, Description: "viewport width px (with height)"},
			"height":            {Type: mcptransport.TypeNumber, Description: "viewport height px (with width)"},
			"deviceScaleFactor": {Type: mcptransport.TypeNumber, Description: "device pixel ratio (default 2 with width/height)"},
			"mobile":            {Type: mcptransport.TypeBoolean, Description: "emulate mobile (with width/height)"},
			"userAgent":         {Type: mcptransport.TypeString, Description: "override the user agent"},
			"latitude":          {Type: mcptransport.TypeNumber, Description: "geolocation latitude (with longitude)"},
			"longitude":         {Type: mcptransport.TypeNumber, Description: "geolocation longitude (with latitude)"},
			"colorScheme":       {Type: mcptransport.TypeString, Description: "prefers-color-scheme", Enum: []string{"light", "dark", "no-preference"}},
			"touch":             {Type: mcptransport.TypeBoolean, Description: "enable touch emulation"},
			"reset":             {Type: mcptransport.TypeBoolean, Description: "clear all emulation overrides instead of setting"},
		},
	},
}

func (c *Core) emulate(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Width             int      `json:"width"`
		Height            int      `json:"height"`
		DeviceScaleFactor float64  `json:"deviceScaleFactor"`
		Mobile            *bool    `json:"mobile"`
		UserAgent         string   `json:"userAgent"`
		Latitude          *float64 `json:"latitude"`
		Longitude         *float64 `json:"longitude"`
		ColorScheme       string   `json:"colorScheme"`
		Touch             *bool    `json:"touch"`
		Reset             bool     `json:"reset"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	page := c.page()

	if a.Reset {
		_ = page.RawCall(ctx, "Emulation.clearDeviceMetricsOverride", nil, nil)
		_ = page.RawCall(ctx, "Emulation.clearGeolocationOverride", nil, nil)
		_ = page.RawCall(ctx, "Emulation.clearUserAgentOverride", nil, nil)
		_ = page.RawCall(ctx, "Emulation.setEmulatedMedia", map[string]any{"features": []any{}}, nil)
		return mcptransport.ResultText("emulation reset"), nil
	}

	var done []string
	if a.Width > 0 && a.Height > 0 {
		dsf := a.DeviceScaleFactor
		if dsf <= 0 {
			dsf = 2
		}
		mobile := false
		if a.Mobile != nil {
			mobile = *a.Mobile
		}
		if err := page.RawCall(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
			"width": a.Width, "height": a.Height, "deviceScaleFactor": dsf, "mobile": mobile,
		}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, fmt.Sprintf("viewport %dx%d dpr=%g mobile=%v", a.Width, a.Height, dsf, mobile))
	}
	if a.UserAgent != "" {
		if err := page.RawCall(ctx, "Emulation.setUserAgentOverride", map[string]any{"userAgent": a.UserAgent}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, "userAgent")
	}
	if a.Latitude != nil && a.Longitude != nil {
		if err := page.RawCall(ctx, "Emulation.setGeolocationOverride", map[string]any{
			"latitude": *a.Latitude, "longitude": *a.Longitude, "accuracy": 1,
		}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, fmt.Sprintf("geolocation %g,%g", *a.Latitude, *a.Longitude))
	}
	if a.ColorScheme != "" {
		if err := page.RawCall(ctx, "Emulation.setEmulatedMedia", map[string]any{
			"features": []any{map[string]any{"name": "prefers-color-scheme", "value": a.ColorScheme}},
		}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, "colorScheme="+a.ColorScheme)
	}
	if a.Touch != nil {
		if err := page.RawCall(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": *a.Touch}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, fmt.Sprintf("touch=%v", *a.Touch))
	}
	if len(done) == 0 {
		return mcptransport.ResultText("no emulation fields set"), nil
	}
	return mcptransport.ResultText("emulating: " + strings.Join(done, ", ")), nil
}
