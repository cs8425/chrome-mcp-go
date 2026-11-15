package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"chrome-mcp/internal/cdp"
)

// parseClip parses a screenshot region string "x,y,width,height" (separators
// , / x / X / * / × / space) into CSS pixel values. width/height must be
// positive; coordinates are page CSS pixels (as CDP Page.captureScreenshot
// expects). ok=false on any malformed or non-positive input.
func parseClip(s string) (x, y, w, h float64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, 0, 0, false
	}
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == 'x' || r == 'X' || r == '*' || r == '\u00d7' || r == ' '
	})
	if len(f) < 4 {
		return 0, 0, 0, 0, false
	}
	vs := make([]float64, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil || v < 0 {
			return 0, 0, 0, 0, false
		}
		vs[i] = v
	}
	if vs[2] <= 0 || vs[3] <= 0 {
		return 0, 0, 0, 0, false
	}
	return vs[0], vs[1], vs[2], vs[3], true
}

// clampScale keeps an output scale within the 0.1-3 band CDP accepts.
func clampScale(s float64) float64 {
	if s < 0.1 {
		return 0.1
	}
	if s > 3 {
		return 3
	}
	return s
}

// viewportBox returns the current visual viewport as a CDP clip box (page CSS
// coordinates): scroll offset plus inner size. Used to apply a scale when no
// explicit clip region was given.
func viewportBox(ctx context.Context, page *cdp.Client) (map[string]any, error) {
	val, err := evalValue(ctx, page, `(function(){return {x:window.scrollX||0,y:window.scrollY||0,w:window.innerWidth,h:window.innerHeight};})()`)
	if err != nil {
		return nil, err
	}
	var b struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if err := json.Unmarshal(val, &b); err != nil {
		return nil, fmt.Errorf("viewport box: %w", err)
	}
	if b.W <= 0 || b.H <= 0 {
		return nil, fmt.Errorf("viewport box: non-positive size %gx%g", b.W, b.H)
	}
	return map[string]any{"x": b.X, "y": b.Y, "width": b.W, "height": b.H}, nil
}

// screenshotMeta renders a short, model-facing summary of what a capture
// actually was (mode, region, scale, output size, format, byte size) so a wrong
// or unexpectedly large/empty capture is debuggable from the text alone, without
// opening the image.
func screenshotMeta(mode, clipStr string, scale float64, hasClip bool, format string, n int, raw []byte) string {
	parts := []string{"mode=" + mode}
	switch {
	case mode == "region":
		parts = append(parts, "clip="+clipStr)
	case hasClip:
		parts = append(parts, "clip=viewport")
	}
	if hasClip && scale != 1 {
		parts = append(parts, fmt.Sprintf("scale=%g", scale))
	}
	if w, h, ok := imageDims(format, raw); ok {
		parts = append(parts, fmt.Sprintf("size=%dx%d", w, h))
	}
	parts = append(parts, "format="+format)
	parts = append(parts, fmt.Sprintf("bytes=%d", n))
	return strings.Join(parts, " ")
}

// imageDims decodes pixel dimensions from encoded image bytes for png, jpeg and
// webp (VP8 lossy, VP8L lossless, VP8X extended). It returns ok=false for an
// unknown format or a header shorter than the fields it needs.
func imageDims(format string, b []byte) (int, int, bool) {
	switch format {
	case "png":
		if len(b) >= 24 && string(b[1:4]) == "PNG" {
			w := int(binary.BigEndian.Uint32(b[16:20]))
			h := int(binary.BigEndian.Uint32(b[20:24]))
			if w > 0 && h > 0 {
				return w, h, true
			}
		}
	case "jpeg":
		if len(b) > 4 && b[0] == 0xFF && b[1] == 0xD8 {
			for i := 2; i+9 < len(b); {
				if b[i] != 0xFF {
					i++
					continue
				}
				m := b[i+1]
				if m == 0x01 || (m >= 0xD0 && m <= 0xD9) { // standalone, no length
					i += 2
					continue
				}
				if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC { // SOF*
					h := int(binary.BigEndian.Uint16(b[i+5 : i+7]))
					w := int(binary.BigEndian.Uint16(b[i+7 : i+9]))
					if w > 0 && h > 0 {
						return w, h, true
					}
				}
				if i+4 > len(b) {
					break
				}
				seg := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
				i += 2 + 2 + seg
			}
		}
	case "webp":
		if len(b) >= 16 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
			switch string(b[12:16]) {
			case "VP8X": // extended: canvas w-1/h-1 are 24-bit LE at offsets 24/27
				if len(b) >= 30 {
					w := (int(b[24]) | int(b[25])<<8 | int(b[26])<<16) + 1
					h := (int(b[27]) | int(b[28])<<8 | int(b[29])<<16) + 1
					if w > 0 && h > 0 {
						return w, h, true
					}
				}
			case "VP8 ": // lossy: sync code 9d 01 2a then 14-bit w/h LE
				if len(b) >= 30 && b[23] == 0x9D && b[24] == 0x01 && b[25] == 0x2A {
					w := int(binary.LittleEndian.Uint16(b[26:28])) & 0x3FFF
					h := int(binary.LittleEndian.Uint16(b[28:30])) & 0x3FFF
					if w > 0 && h > 0 {
						return w, h, true
					}
				}
			case "VP8L": // lossless: signature 0x2f then 14-bit w-1/h-1 packed LE
				if len(b) >= 25 && b[20] == 0x2F {
					bits := uint32(b[21]) | uint32(b[22])<<8 | uint32(b[23])<<16 | uint32(b[24])<<24
					w := int(bits&0x3FFF) + 1
					h := int((bits>>14)&0x3FFF) + 1
					if w > 0 && h > 0 {
						return w, h, true
					}
				}
			}
		}
	}
	return 0, 0, false
}
