package tools

// The dev pack (cont.): emulate. Device metrics, user agent, geolocation,
// media features, extra HTTP headers, and CPU/network throttling via the
// Emulation/Network domains. Only the fields you pass are applied; reset=true
// clears all overrides.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mcptransport"
)

var emulateTool = &mcptransport.Tool{
	Name:        "emulate",
	Description: "Emulate device/UA/geolocation/media/headers/throttling on the current tab. Set only the fields you pass; pass reset=true to clear all overrides. width+height (or a compressed viewport string like '375x667x2,mobile') set the viewport. colorScheme is light|dark|no-preference. extraHttpHeaders injects headers into every request ({} clears). networkConditions is a preset (slow-3g|fast-3g|slow-4g|fast-4g|offline|none). cpuThrottlingRate slows the CPU (1 = normal).",
	InputSchema: mcptransport.Schema{
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"width":                {Type: mcptransport.TypeNumber, Description: "viewport width px (with height)"},
			"height":               {Type: mcptransport.TypeNumber, Description: "viewport height px (with width)"},
			"deviceScaleFactor":    {Type: mcptransport.TypeNumber, Description: "device pixel ratio (default 2 with width/height)"},
			"mobile":               {Type: mcptransport.TypeBoolean, Description: "emulate mobile (with width/height)"},
			"viewport":             {Type: mcptransport.TypeString, Description: "compressed viewport shorthand: '375x667', '375x667x2', or '375x667x2,mobile'"},
			"userAgent":            {Type: mcptransport.TypeString, Description: "override the user agent"},
			"latitude":             {Type: mcptransport.TypeNumber, Description: "geolocation latitude (with longitude)"},
			"longitude":            {Type: mcptransport.TypeNumber, Description: "geolocation longitude (with latitude)"},
			"colorScheme":          {Type: mcptransport.TypeString, Description: "prefers-color-scheme", Enum: []string{"light", "dark", "no-preference"}},
			"prefersReducedMotion": {Type: mcptransport.TypeString, Description: "prefers-reduced-motion", Enum: []string{"no-preference", "reduce"}},
			"forcedColors":         {Type: mcptransport.TypeString, Description: "forced-colors", Enum: []string{"none", "active"}},
			"prefersContrast":      {Type: mcptransport.TypeString, Description: "prefers-contrast", Enum: []string{"no-preference", "more", "less", "custom"}},
			"extraHttpHeaders":     {Type: mcptransport.TypeObject, Description: "headers injected into every request (e.g. {\"Authorization\":\"Bearer …\"}); pass {} to clear"},
			"networkConditions":    {Type: mcptransport.TypeString, Description: "network throttle preset", Enum: []string{"slow-3g", "fast-3g", "slow-4g", "fast-4g", "offline", "none"}},
			"cpuThrottlingRate":    {Type: mcptransport.TypeNumber, Description: "CPU slowdown factor (1 = normal, e.g. 4 = 4x slower)"},
			"touch":                {Type: mcptransport.TypeBoolean, Description: "enable touch emulation"},
			"reset":                {Type: mcptransport.TypeBoolean, Description: "clear all emulation overrides instead of setting"},
		},
	},
}

func (c *Core) emulate(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Width                int                `json:"width"`
		Height               int                `json:"height"`
		DeviceScaleFactor    float64            `json:"deviceScaleFactor"`
		Mobile               *bool              `json:"mobile"`
		Viewport             string             `json:"viewport"`
		UserAgent            string             `json:"userAgent"`
		Latitude             *float64           `json:"latitude"`
		Longitude            *float64           `json:"longitude"`
		ColorScheme          string             `json:"colorScheme"`
		PrefersReducedMotion string             `json:"prefersReducedMotion"`
		ForcedColors         string             `json:"forcedColors"`
		PrefersContrast      string             `json:"prefersContrast"`
		ExtraHTTPHeaders     *map[string]string `json:"extraHttpHeaders"`
		NetworkConditions    string             `json:"networkConditions"`
		CPUThrottlingRate    float64            `json:"cpuThrottlingRate"`
		Touch                *bool              `json:"touch"`
		Reset                bool               `json:"reset"`
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
		_ = page.RawCall(ctx, "Emulation.setCPUThrottlingRate", map[string]any{"rate": 1}, nil)
		_ = page.RawCall(ctx, "Network.enable", nil, nil)
		_ = page.RawCall(ctx, "Network.emulateNetworkConditions", map[string]any{
			"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1,
		}, nil)
		_ = page.RawCall(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}}, nil)
		return mcptransport.ResultText("emulation reset"), nil
	}

	var done []string

	// Viewport: explicit width/height, or a compressed shorthand.
	w, h := a.Width, a.Height
	dsf := a.DeviceScaleFactor
	mobile := false
	if a.Mobile != nil {
		mobile = *a.Mobile
	}
	if a.Viewport != "" {
		vw, vh, vd, vm, ok := parseViewport(a.Viewport)
		if !ok {
			return mcptransport.ResultError("bad viewport: " + a.Viewport + " (want e.g. 375x667x2,mobile)"), nil
		}
		w, h = vw, vh
		if vd > 0 {
			dsf = vd
		}
		if vm {
			mobile = true
		}
	}
	if w > 0 && h > 0 {
		if dsf <= 0 {
			dsf = 2
		}
		if err := page.RawCall(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
			"width": w, "height": h, "deviceScaleFactor": dsf, "mobile": mobile,
		}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, fmt.Sprintf("viewport %dx%d dpr=%g mobile=%v", w, h, dsf, mobile))
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

	// Media features are set as one batch (setEmulatedMedia replaces all).
	var feats []any
	if a.ColorScheme != "" {
		feats = append(feats, map[string]any{"name": "prefers-color-scheme", "value": a.ColorScheme})
	}
	if a.PrefersReducedMotion != "" {
		feats = append(feats, map[string]any{"name": "prefers-reduced-motion", "value": a.PrefersReducedMotion})
	}
	if a.ForcedColors != "" {
		feats = append(feats, map[string]any{"name": "forced-colors", "value": a.ForcedColors})
	}
	if a.PrefersContrast != "" {
		feats = append(feats, map[string]any{"name": "prefers-contrast", "value": a.PrefersContrast})
	}
	if len(feats) > 0 {
		if err := page.RawCall(ctx, "Emulation.setEmulatedMedia", map[string]any{"features": feats}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, fmt.Sprintf("media(%d)", len(feats)))
	}

	if a.ExtraHTTPHeaders != nil {
		if err := page.RawCall(ctx, "Network.enable", nil, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if err := page.RawCall(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": *a.ExtraHTTPHeaders}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, fmt.Sprintf("extraHttpHeaders(%d)", len(*a.ExtraHTTPHeaders)))
	}

	if a.NetworkConditions != "" {
		p, ok := netPreset(a.NetworkConditions)
		if !ok {
			return mcptransport.ResultError("unknown networkConditions: " + a.NetworkConditions), nil
		}
		if err := page.RawCall(ctx, "Network.enable", nil, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		if err := page.RawCall(ctx, "Network.emulateNetworkConditions", map[string]any{
			"offline": p.offline, "latency": p.latency,
			"downloadThroughput": p.dl, "uploadThroughput": p.ul,
		}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, "network="+a.NetworkConditions)
	}

	if a.CPUThrottlingRate > 0 {
		if err := page.RawCall(ctx, "Emulation.setCPUThrottlingRate", map[string]any{"rate": a.CPUThrottlingRate}, nil); err != nil {
			return mcptransport.ResultError(err.Error()), nil
		}
		done = append(done, fmt.Sprintf("cpu=%.1gx", a.CPUThrottlingRate))
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

// parseViewport parses a compressed viewport string such as "375x667",
// "375x667x2", or "375x667x2,mobile" (case-insensitive; "m" == mobile). The
// device-scale-factor segment is integer only.
func parseViewport(s string) (w, h int, dsf float64, mobile bool, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, 0, 0, false, false
	}
	parts := strings.Split(s, ",")
	for _, opt := range parts[1:] {
		switch strings.TrimSpace(opt) {
		case "mobile", "m":
			mobile = true
		}
	}
	f := strings.FieldsFunc(parts[0], func(r rune) bool {
		return r == 'x' || r == '*' || r == '×'
	})
	if len(f) < 2 {
		return 0, 0, 0, mobile, false
	}
	if _, err := fmt.Sscanf(f[0]+" "+f[1], "%d %d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, 0, mobile, false
	}
	if len(f) >= 3 {
		var d int
		if _, err := fmt.Sscanf(f[2], "%d", &d); err == nil && d > 0 {
			dsf = float64(d)
		}
	}
	return w, h, dsf, mobile, true
}

// netProfile is a Network.emulateNetworkConditions parameter set. dl/ul are
// bytes/sec; -1 means unthrottled.
type netProfile struct {
	offline bool
	latency float64 // ms
	dl      float64
	ul      float64
}

func netPreset(name string) (netProfile, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "offline":
		return netProfile{offline: true, dl: 0, ul: 0}, true
	case "slow-3g":
		return netProfile{latency: 2000, dl: 50000, ul: 50000}, true
	case "fast-3g":
		return netProfile{latency: 562, dl: 150000, ul: 100000}, true
	case "slow-4g", "4g":
		return netProfile{latency: 250, dl: 1000000, ul: 750000}, true
	case "fast-4g":
		return netProfile{latency: 50, dl: 4000000, ul: 2000000}, true
	case "none", "off", "no-emulation":
		return netProfile{dl: -1, ul: -1}, true
	}
	return netProfile{}, false
}
