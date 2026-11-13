package tools

// The browse pack (cont.): interact — a batched gesture sequencer and
// single-action runner. It turns either a list of steps (click/hover/drag/
// swipe/type/key/scroll/wait) or one inline action (press_key-style, e.g.
// {key:"Enter"}) into real Input-domain events. Pointer moves are interpolated
// (linear / ease / cubic-bezier) with optional human-like jitter, so drag/hover/
// swipe actually grab and move (Chrome needs the intermediate move events).
// Multi-touch gestures (pinch/rotate) build on the same engine in a later phase.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"chrome-mcp/internal/cdp"
	"mcptransport"
)

// ---------- data model ----------

type pt struct{ x, y float64 }

type motion struct {
	Curve    string  `json:"curve"`    // linear|easeIn|easeOut|easeInOut|cubic-bezier(x1,y1,x2,y2)
	Duration float64 `json:"duration"` // ms
	Steps    int     `json:"steps"`    // sample count (else derived from duration)
	Jitter   float64 `json:"jitter"`   // px perpendicular noise
	Hold     float64 `json:"hold"`     // ms press dwell before moving
	Seed     int64   `json:"seed"`     // deterministic jitter when set
}

type interactStep struct {
	Do       string   `json:"do"`
	Kind     string   `json:"kind"` // alias for Do
	Selector string   `json:"selector"`
	At       string   `json:"at"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Double   bool     `json:"double"`
	Button   string   `json:"button"`
	Text     string   `json:"text"`
	Key      string   `json:"key"`
	Mods     []string `json:"mods"`
	DX       float64  `json:"dx"`
	DY       float64  `json:"dy"`
	MS       float64  `json:"ms"`
	Gone     bool     `json:"gone"`
	URL      string   `json:"url"`
	Timeout  float64  `json:"timeout"`
	On       string   `json:"on"`    // pinch/rotate center
	Scale    float64  `json:"scale"` // pinch: end/start finger radius ratio
	Angle    float64  `json:"angle"` // rotate: degrees
	Motion   *motion  `json:"motion"`
}

func motionOrDefault(mp *motion) motion {
	if mp == nil {
		return motion{Curve: "easeInOut", Duration: 250, Steps: 16, Jitter: 1.0, Hold: 40}
	}
	m := *mp
	if m.Curve == "" {
		m.Curve = "easeInOut"
	}
	if m.Duration <= 0 {
		m.Duration = 250
	}
	if m.Steps <= 0 {
		m.Steps = int(m.Duration / 16)
	}
	if m.Steps < 2 {
		m.Steps = 2
	}
	if m.Steps > 500 {
		m.Steps = 500
	}
	return m
}

// inferDo picks an action from the fields when "do" is omitted (single-action
// convenience). Precedence follows the most specific field present.
func inferDo(st *interactStep) string {
	switch {
	case st.Key != "":
		return "key"
	case st.Text != "":
		return "type"
	case st.Scale > 0:
		return "pinch"
	case st.Angle != 0:
		return "rotate"
	case st.From != "" || st.To != "":
		return "drag"
	case st.DX != 0 || st.DY != 0:
		return "scroll"
	case st.MS > 0 || st.URL != "":
		return "wait"
	case st.Selector != "" || st.At != "":
		return "click"
	default:
		return ""
	}
}

// isEmptyAction reports whether a single-action payload carries no usable
// action at all (so we can return a helpful error instead of a no-op).
func isEmptyAction(st *interactStep) bool {
	return st.Do == "" && st.Kind == "" && st.Selector == "" && st.At == "" && st.On == "" &&
		st.From == "" && st.To == "" && st.Text == "" && st.Key == "" && st.URL == "" &&
		st.DX == 0 && st.DY == 0 && st.MS <= 0 && st.Scale == 0 && st.Angle == 0
}

// ---------- easing ----------

func ease(curve string, t float64) float64 {
	switch curve {
	case "linear":
		return t
	case "easeIn":
		return t * t
	case "easeOut":
		return t * (2 - t)
	case "easeInOut":
		return t * t * (3 - 2*t)
	}
	if strings.HasPrefix(curve, "cubic-bezier(") {
		if x1, y1, x2, y2, ok := parseBezier(curve); ok {
			return cubicBezier(x1, y1, x2, y2, t)
		}
	}
	return t * t * (3 - 2*t) // fall back to easeInOut
}

func parseBezier(s string) (x1, y1, x2, y2 float64, ok bool) {
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "cubic-bezier("), ")")
	parts := strings.Split(inner, ",")
	if len(parts) != 4 {
		return 0, 0, 0, 0, false
	}
	var fs [4]float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		fs[i] = v
	}
	return fs[0], fs[1], fs[2], fs[3], true
}

// cubicBezier evaluates CSS cubic-bezier(x1,y1,x2,y2): the y for progress x.
func cubicBezier(x1, y1, x2, y2, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	comp := func(a, b, u float64) float64 {
		inv := 1 - u
		return 3*inv*inv*u*a + 3*inv*u*u*b + u*u*u
	}
	lo, hi, u := 0.0, 1.0, x
	for i := 0; i < 24; i++ {
		cx := comp(x1, x2, u)
		if math.Abs(cx-x) < 1e-5 {
			break
		}
		if cx < x {
			lo = u
		} else {
			hi = u
		}
		u = (lo + hi) / 2
	}
	return comp(y1, y2, u)
}

// interpPath samples the eased path from->to (excluding endpoints) with jitter.
func interpPath(from, to pt, m motion, rnd *rand.Rand) []pt {
	n := m.Steps
	if n < 2 {
		n = 2
	}
	dx, dy := to.x-from.x, to.y-from.y
	l := math.Hypot(dx, dy)
	var nx, ny float64
	if l > 0 {
		nx, ny = -dy/l, dx/l
	}
	out := make([]pt, 0, n-1)
	for i := 1; i < n; i++ {
		t := float64(i) / float64(n)
		e := ease(m.Curve, t)
		x := from.x + dx*e
		y := from.y + dy*e
		if m.Jitter > 0 && l > 0 && rnd != nil {
			amp := (rnd.Float64()*2 - 1) * m.Jitter * math.Sin(math.Pi*t)
			x += nx * amp
			y += ny * amp
		}
		out = append(out, pt{x, y})
	}
	return out
}

// ---------- low-level input ----------

type touchPt struct {
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	ID int64   `json:"id"`
}

func touchEvent(ctx context.Context, page *cdp.Client, phase string, pts []touchPt) error {
	if pts == nil {
		pts = []touchPt{}
	}
	return page.RawCall(ctx, "Input.dispatchTouchEvent", map[string]any{"type": phase, "touchPoints": pts}, nil)
}

func dispatchKey(ctx context.Context, page *cdp.Client, phase, key, code string, vk int, text string, mods int) error {
	p := map[string]any{"type": phase, "key": key, "modifiers": mods}
	if code != "" {
		p["code"] = code
	}
	if vk != 0 {
		p["keyCode"] = vk
		p["windowsVirtualKeyCode"] = vk
		p["nativeVirtualKeyCode"] = vk
	}
	if text != "" {
		p["text"] = text
	}
	return page.RawCall(ctx, "Input.dispatchKeyEvent", p, nil)
}

func sleepCtx(ctx context.Context, ms float64) {
	if ms <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(ms * float64(time.Millisecond))):
	}
}

// ---------- key map ----------

type keySpec struct {
	key  string
	code string
	vk   int
}

var keymap = buildKeymap()

func buildKeymap() map[string]keySpec {
	m := map[string]keySpec{
		"enter":      {"Enter", "Enter", 13},
		"tab":        {"Tab", "Tab", 9},
		"escape":     {"Escape", "Escape", 27},
		"space":      {" ", "Space", 32},
		" ":          {" ", "Space", 32},
		"backspace":  {"Backspace", "Backspace", 8},
		"delete":     {"Delete", "Delete", 46},
		"insert":     {"Insert", "Insert", 45},
		"home":       {"Home", "Home", 36},
		"end":        {"End", "End", 35},
		"pageup":     {"PageUp", "PageUp", 33},
		"pagedown":   {"PageDown", "PageDown", 34},
		"arrowup":    {"ArrowUp", "ArrowUp", 38},
		"arrowdown":  {"ArrowDown", "ArrowDown", 40},
		"arrowleft":  {"ArrowLeft", "ArrowLeft", 37},
		"arrowright": {"ArrowRight", "ArrowRight", 39},
	}
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("f%d", i+1)
		up := strings.ToUpper(name)
		m[name] = keySpec{up, up, 112 + i}
	}
	for a, target := range map[string]string{
		"return": "enter", "esc": "escape", "del": "delete",
		"pgup": "pageup", "pgdn": "pagedown",
		"up": "arrowup", "down": "arrowdown", "left": "arrowleft", "right": "arrowright",
	} {
		if spec, ok := m[target]; ok {
			m[a] = spec
		}
	}
	return m
}

func normKeyName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func modsMask(mods []string) int {
	var m int
	for _, x := range mods {
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "alt", "option":
			m |= 1
		case "ctrl", "control":
			m |= 2
		case "meta", "cmd", "command", "super", "win":
			m |= 4
		case "shift":
			m |= 8
		}
	}
	return m
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveTarget resolves a step endpoint given as either "x,y" or a CSS
// selector (→ element center) into viewport CSS coordinates.
func (c *Core) resolveTarget(ctx context.Context, page *cdp.Client, s string) (float64, float64, error) {
	if s == "" {
		return 0, 0, fmt.Errorf("empty target")
	}
	if x, y, ok := parsePoint(s); ok {
		return x, y, nil
	}
	_, id, err := c.resolveNode(ctx, s)
	if err != nil {
		return 0, 0, err
	}
	return nodeCenter(ctx, page, id)
}

// resolveTargetOrCenter resolves s if non-empty, else falls back to the
// viewport center (used by pinch/rotate when no center is given).
func (c *Core) resolveTargetOrCenter(ctx context.Context, page *cdp.Client, s string) (float64, float64, error) {
	if s != "" {
		return c.resolveTarget(ctx, page, s)
	}
	return viewportCenter(ctx, page)
}

// enableTouch turns on touch event dispatch so Input.dispatchTouchEvent with
// multiple points is delivered to the page.
func enableTouch(ctx context.Context, page *cdp.Client) error {
	return page.RawCall(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true, "maxTouchPoints": 4}, nil)
}

func toTouchPts(pts []pt) []touchPt {
	out := make([]touchPt, len(pts))
	for i, p := range pts {
		out[i] = touchPt{X: p.x, Y: p.y, ID: int64(i)}
	}
	return out
}

// driveMultiTouch drives N touch points in lockstep: touchStart, eased
// touchMove frames from track(e), then touchEnd. track maps progress 0..1 to
// the current points (same order/length as start).
func (c *Core) driveMultiTouch(ctx context.Context, page *cdp.Client, start []pt, track func(float64) []pt, m motion) (string, error) {
	if len(start) < 1 {
		return "", fmt.Errorf("multi-touch needs at least one point")
	}
	if err := enableTouch(ctx, page); err != nil {
		return "", err
	}
	if err := touchEvent(ctx, page, "touchStart", toTouchPts(start)); err != nil {
		return "", err
	}
	sleepCtx(ctx, m.Hold)
	n := m.Steps
	for s := 1; s < n; s++ {
		t := float64(s) / float64(n)
		e := ease(m.Curve, t)
		if err := touchEvent(ctx, page, "touchMove", toTouchPts(track(e))); err != nil {
			return "", err
		}
	}
	if err := touchEvent(ctx, page, "touchEnd", toTouchPts(track(1))); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d fingers, %d frames", len(start), n-1), nil
}

// ---------- step runner ----------

func (c *Core) runStep(ctx context.Context, page *cdp.Client, st *interactStep, rnd *rand.Rand) (string, error) {
	switch st.Do {
	case "click", "tap":
		x, y, err := c.resolveTarget(ctx, page, firstNonEmpty(st.At, st.Selector))
		if err != nil {
			return "", err
		}
		b := st.Button
		if b == "" || st.Do == "tap" {
			b = "left"
		}
		_ = mouse(ctx, page, "mouseMoved", x, y, "", 0)
		if st.Double {
			_ = mouse(ctx, page, "mousePressed", x, y, b, 1)
			_ = mouse(ctx, page, "mouseReleased", x, y, b, 1)
			if err := mouse(ctx, page, "mousePressed", x, y, b, 2); err != nil {
				return "", err
			}
			if err := mouse(ctx, page, "mouseReleased", x, y, b, 2); err != nil {
				return "", err
			}
		} else {
			if err := mouse(ctx, page, "mousePressed", x, y, b, 1); err != nil {
				return "", err
			}
			if err := mouse(ctx, page, "mouseReleased", x, y, b, 1); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("%d,%d", int(x), int(y)), nil

	case "hover", "move":
		x, y, err := c.resolveTarget(ctx, page, firstNonEmpty(st.At, st.Selector))
		if err != nil {
			return "", err
		}
		if err := mouse(ctx, page, "mouseMoved", x, y, "", 0); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d,%d", int(x), int(y)), nil

	case "drag":
		fx, fy, err := c.resolveTarget(ctx, page, st.From)
		if err != nil {
			return "", err
		}
		tx, ty, err := c.resolveTarget(ctx, page, st.To)
		if err != nil {
			return "", err
		}
		m := motionOrDefault(st.Motion)
		from, to := pt{fx, fy}, pt{tx, ty}
		_ = mouse(ctx, page, "mouseMoved", from.x, from.y, "", 0)
		if err := mouse(ctx, page, "mousePressed", from.x, from.y, "left", 1); err != nil {
			return "", err
		}
		sleepCtx(ctx, m.Hold)
		for _, p := range interpPath(from, to, m, seededRand(m, rnd)) {
			if err := mouse(ctx, page, "mouseMoved", p.x, p.y, "left", 1); err != nil {
				return "", err
			}
		}
		if err := mouse(ctx, page, "mouseReleased", to.x, to.y, "left", 1); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d,%d->%d,%d (%d pts)", int(from.x), int(from.y), int(to.x), int(to.y), m.Steps-1), nil

	case "swipe":
		fx, fy, err := c.resolveTarget(ctx, page, st.From)
		if err != nil {
			return "", err
		}
		tx, ty, err := c.resolveTarget(ctx, page, st.To)
		if err != nil {
			return "", err
		}
		m := motionOrDefault(st.Motion)
		from, to := pt{fx, fy}, pt{tx, ty}
		if err := touchEvent(ctx, page, "touchStart", []touchPt{{X: from.x, Y: from.y, ID: 0}}); err != nil {
			return "", err
		}
		sleepCtx(ctx, m.Hold)
		for _, p := range interpPath(from, to, m, seededRand(m, rnd)) {
			if err := touchEvent(ctx, page, "touchMove", []touchPt{{X: p.x, Y: p.y, ID: 0}}); err != nil {
				return "", err
			}
		}
		if err := touchEvent(ctx, page, "touchEnd", []touchPt{}); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d,%d->%d,%d (%d pts)", int(from.x), int(from.y), int(to.x), int(to.y), m.Steps-1), nil

	case "type":
		if st.Selector != "" {
			_, id, err := c.resolveNode(ctx, st.Selector)
			if err != nil {
				return "", err
			}
			if err := page.RawCall(ctx, "DOM.focus", map[string]any{"nodeId": id}, nil); err != nil {
				return "", err
			}
		}
		if st.Text == "" {
			return "no text", nil
		}
		if err := page.RawCall(ctx, "Input.insertText", map[string]any{"text": st.Text}, nil); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d chars", len([]rune(st.Text))), nil

	case "key":
		return c.doKeyStep(ctx, page, st)

	case "scroll":
		if st.To != "" {
			if _, _, err := c.resolveNode(ctx, st.To); err != nil {
				return "", err
			}
			return "scrolled to " + st.To, nil
		}
		if st.DX == 0 && st.DY == 0 {
			return "", fmt.Errorf("scroll step needs to, or dx/dy")
		}
		x, y, err := viewportCenter(ctx, page)
		if err != nil {
			return "", err
		}
		if st.At != "" {
			if px, py, ok := parsePoint(st.At); ok {
				x, y = px, py
			}
		}
		if err := wheel(ctx, page, x, y, st.DX, st.DY); err != nil {
			return "", err
		}
		return fmt.Sprintf("dx=%g dy=%g", st.DX, st.DY), nil

	case "wait":
		return c.interactWait(ctx, st)

	case "pinch":
		cx, cy, err := c.resolveTargetOrCenter(ctx, page, firstNonEmpty(st.At, st.Selector, st.On))
		if err != nil {
			return "", err
		}
		scale := st.Scale
		if scale <= 0 {
			scale = 2
		}
		R := 100.0
		start := []pt{{cx + R, cy}, {cx - R, cy}}
		track := func(e float64) []pt {
			r := R * (1 + (scale-1)*e)
			return []pt{{cx + r, cy}, {cx - r, cy}}
		}
		return c.driveMultiTouch(ctx, page, start, track, motionOrDefault(st.Motion))

	case "rotate":
		if st.Angle == 0 {
			return "", fmt.Errorf("rotate needs angle (degrees)")
		}
		cx, cy, err := c.resolveTargetOrCenter(ctx, page, firstNonEmpty(st.At, st.Selector, st.On))
		if err != nil {
			return "", err
		}
		R := 100.0
		theta := st.Angle * math.Pi / 180
		start := []pt{{cx + R, cy}, {cx - R, cy}}
		track := func(e float64) []pt {
			a := theta * e
			cc, ss := math.Cos(a), math.Sin(a)
			return []pt{{cx + R*cc, cy + R*ss}, {cx - R*cc, cy - R*ss}}
		}
		return c.driveMultiTouch(ctx, page, start, track, motionOrDefault(st.Motion))

	default:
		return "", fmt.Errorf("unknown step do: %q", st.Do)
	}
}

func (c *Core) doKeyStep(ctx context.Context, page *cdp.Client, st *interactStep) (string, error) {
	name := strings.TrimSpace(st.Key)
	if name == "" {
		return "", fmt.Errorf("key step needs key")
	}
	mods := modsMask(st.Mods)
	if spec, ok := keymap[normKeyName(name)]; ok {
		if err := dispatchKey(ctx, page, "keyDown", spec.key, spec.code, spec.vk, "", mods); err != nil {
			return "", err
		}
		if err := dispatchKey(ctx, page, "keyUp", spec.key, spec.code, spec.vk, "", mods); err != nil {
			return "", err
		}
		return st.Key, nil
	}
	if rs := []rune(name); len(rs) == 1 {
		ch := string(rs)
		if err := dispatchKey(ctx, page, "keyDown", ch, "", 0, ch, mods); err != nil {
			return "", err
		}
		if err := dispatchKey(ctx, page, "keyUp", ch, "", 0, ch, mods); err != nil {
			return "", err
		}
		return "char " + ch, nil
	}
	return "", fmt.Errorf("unknown key %q; use do:%q for text", st.Key, "type")
}

func (c *Core) interactWait(ctx context.Context, st *interactStep) (string, error) {
	if st.MS > 0 {
		d := time.Duration(st.MS * float64(time.Millisecond))
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(d):
		}
		return d.String(), nil
	}
	wa := &waitArgs{Selector: st.Selector, Gone: st.Gone, Text: st.Text, URL: st.URL}
	switch {
	case st.Selector != "":
		wa.Action = "selector"
	case st.Text != "":
		wa.Action = "text"
	case st.URL != "":
		wa.Action = "url"
	default:
		return "", fmt.Errorf("wait step needs ms, selector, text, or url")
	}
	timeout := st.Timeout
	if timeout <= 0 {
		timeout = 8
	}
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		ok, err := c.waitCondition(ctx, wa)
		if err != nil {
			return "", err
		}
		if ok {
			return wa.Action + " met", nil
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("timeout after %gs waiting for %s", timeout, waitDesc(wa))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func seededRand(m motion, fallback *rand.Rand) *rand.Rand {
	if m.Seed != 0 {
		return rand.New(rand.NewSource(m.Seed))
	}
	return fallback
}

// ---------- tool ----------

// interactStepProps describes the fields of a single step. It is reused both as
// the items schema of "steps" and merged into the top level for single-action
// mode, so the two stay in sync.
var interactStepProps = map[string]mcptransport.Schema{
	"do":       {Type: mcptransport.TypeString, Description: "click|tap|hover|move|drag|swipe|pinch|rotate|type|key|scroll|wait (inferred from the fields if omitted)", Enum: []string{"click", "tap", "hover", "move", "drag", "swipe", "pinch", "rotate", "type", "key", "scroll", "wait"}},
	"kind":     {Type: mcptransport.TypeString, Description: `alias for "do"`},
	"selector": {Type: mcptransport.TypeString, Description: "element selector (click/hover/type focus)"},
	"at":       {Type: mcptransport.TypeString, Description: `viewport point "x,y" or a selector (click/hover/scroll origin)`},
	"from":     {Type: mcptransport.TypeString, Description: "drag/swipe start: selector or \"x,y\""},
	"to":       {Type: mcptransport.TypeString, Description: "drag/swipe end (or scroll target): selector or \"x,y\""},
	"on":       {Type: mcptransport.TypeString, Description: "pinch/rotate center: selector or \"x,y\" (default: viewport center)"},
	"scale":    {Type: mcptransport.TypeNumber, Description: "pinch: end/start finger radius ratio (2 = spread/zoom in, 0.5 = pinch in)"},
	"angle":    {Type: mcptransport.TypeNumber, Description: "rotate: rotation angle in degrees"},
	"double":   {Type: mcptransport.TypeBoolean, Description: "click: double click"},
	"button":   {Type: mcptransport.TypeString, Description: "click button: left|right|middle", Enum: []string{"left", "right", "middle"}},
	"text":     {Type: mcptransport.TypeString, Description: "type: text to insert (any Unicode; uses Input.insertText)"},
	"key":      {Type: mcptransport.TypeString, Description: "key: key name (Enter, Tab, Escape, ArrowUp, F1-F12, ...) or a single char"},
	"mods":     {Type: mcptransport.TypeArray, Items: &mcptransport.Schema{Type: mcptransport.TypeString}, Description: "key modifiers: ctrl|shift|alt|meta"},
	"dx":       {Type: mcptransport.TypeNumber, Description: "scroll: horizontal delta px"},
	"dy":       {Type: mcptransport.TypeNumber, Description: "scroll: vertical delta px (positive=down)"},
	"ms":       {Type: mcptransport.TypeNumber, Description: "wait: fixed milliseconds"},
	"gone":     {Type: mcptransport.TypeBoolean, Description: "wait: wait for selector/text to be absent"},
	"url":      {Type: mcptransport.TypeString, Description: "wait: URL substring to appear"},
	"timeout":  {Type: mcptransport.TypeNumber, Description: "wait: deadline seconds (default 8)"},
	"motion": {
		Type: mcptransport.TypeObject,
		Properties: map[string]mcptransport.Schema{
			"curve":    {Type: mcptransport.TypeString, Description: "linear|easeIn|easeOut|easeInOut|cubic-bezier(x1,y1,x2,y2)"},
			"duration": {Type: mcptransport.TypeNumber, Description: "ms (default 250)"},
			"steps":    {Type: mcptransport.TypeInteger, Description: "sample count (else derived from duration at ~60fps)"},
			"jitter":   {Type: mcptransport.TypeNumber, Description: "px perpendicular noise (default 1)"},
			"hold":     {Type: mcptransport.TypeNumber, Description: "ms press dwell before moving"},
			"seed":     {Type: mcptransport.TypeInteger, Description: "deterministic jitter when set"},
		},
	},
}

func interactTopProps() map[string]mcptransport.Schema {
	m := make(map[string]mcptransport.Schema, len(interactStepProps)+2)
	for k, v := range interactStepProps {
		m[k] = v
	}
	m["steps"] = mcptransport.Schema{
		Type:        mcptransport.TypeArray,
		Description: "batched sequence of steps; when present it takes precedence over the top-level single-action fields",
		Items:       &mcptransport.Schema{Type: mcptransport.TypeObject, Properties: interactStepProps},
	}
	m["stopOnError"] = mcptransport.Schema{Type: mcptransport.TypeBoolean, Description: "stop at first failing step (default true)"}
	return m
}

var interactTool = &mcptransport.Tool{
	Name:        "interact",
	Description: "Run interaction on the current tab, either a batched steps[] sequence or ONE inline action given directly (press_key-style): {key:\"Enter\"}, {text:\"hi\"}, {selector:\"#x\"}=click, {from,to}=drag, {dx,dy}=scroll, {ms}=wait. Each step (or the single action) names a \"do\": click|tap|hover|move|drag|swipe|type|key|scroll|wait — inferred from the fields if omitted. Endpoints (at/from/to) are a CSS selector or an \"x,y\" point. Motion (drag/swipe) uses an easing curve (linear|easeIn|easeOut|easeInOut|cubic-bezier(x1,y1,x2,y2)), duration, optional jitter and seed; omit for human-like defaults. key uses a name (Enter, Tab, Escape, arrows, F1-F12, ...) with optional mods; a single printable char also works, otherwise use type. pinch {scale} and rotate {angle} drive two touch points around a center (on/at, default viewport center). stopOnError defaults true.",
	InputSchema: mcptransport.Schema{
		Type:       mcptransport.TypeObject,
		Properties: interactTopProps(),
	},
}

func (c *Core) interact(ctx context.Context, call *mcptransport.Call) (*mcptransport.ToolResult, error) {
	var a struct {
		Steps       []interactStep `json:"steps"`
		StopOnError *bool          `json:"stopOnError"`
		interactStep
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return mcptransport.ResultError("bad arguments: " + err.Error()), nil
	}
	steps := a.Steps
	if len(steps) == 0 {
		if isEmptyAction(&a.interactStep) {
			return mcptransport.ResultError(`interact needs "steps", or a single action (e.g. {key:"Enter"}, {text:"hi"}, {selector:"#x"}, {from,to}, {dx,dy}, {ms})`), nil
		}
		steps = []interactStep{a.interactStep}
	}
	stopOnError := true
	if a.StopOnError != nil {
		stopOnError = *a.StopOnError
	}
	page := c.page()
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	var lines []string
	failed := false
	for i := range steps {
		st := &steps[i]
		if st.Do == "" {
			st.Do = st.Kind
		}
		if st.Do == "" {
			st.Do = inferDo(st)
		}
		detail, err := c.runStep(ctx, page, st, rnd)
		if err != nil {
			failed = true
			lines = append(lines, fmt.Sprintf("step %d %s: ERROR %s", i+1, st.Do, err.Error()))
			if stopOnError {
				break
			}
			continue
		}
		lines = append(lines, fmt.Sprintf("step %d %s: ok %s", i+1, st.Do, detail))
	}
	status := "done"
	if failed {
		if stopOnError {
			status = "stopped on error"
		} else {
			status = "completed with errors"
		}
	}
	return mcptransport.ResultText(fmt.Sprintf("%s (%d steps)\n%s", status, len(lines), strings.Join(lines, "\n"))), nil
}
