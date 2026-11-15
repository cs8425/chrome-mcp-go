package tools

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestParseClip(t *testing.T) {
	cases := []struct {
		in         string
		ok         bool
		x, y, w, h float64
	}{
		{"10,20,300,400", true, 10, 20, 300, 400},
		{"100x200x30x40", true, 100, 200, 30, 40},
		{"0,0,640,480", true, 0, 0, 640, 480},
		{"  1.5 , 2.5 , 30 , 40 ", true, 1.5, 2.5, 30, 40},
		{"1,2,0,10", false, 0, 0, 0, 0},  // zero width
		{"1,2,10,-3", false, 0, 0, 0, 0}, // negative height
		{"-1,0,10,10", false, 0, 0, 0, 0},
		{"1,2,3", false, 0, 0, 0, 0},
		{"", false, 0, 0, 0, 0},
		{"a,b,c,d", false, 0, 0, 0, 0},
	}
	for _, c := range cases {
		x, y, w, h, ok := parseClip(c.in)
		if ok != c.ok {
			t.Errorf("parseClip(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && (x != c.x || y != c.y || w != c.w || h != c.h) {
			t.Errorf("parseClip(%q) = %g,%g,%g,%g, want %g,%g,%g,%g", c.in, x, y, w, h, c.x, c.y, c.w, c.h)
		}
	}
}

func TestClampScale(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0.01, 0.1}, {0.1, 0.1}, {1.5, 1.5}, {3, 3}, {10, 3},
	} {
		if got := clampScale(c.in); got != c.want {
			t.Errorf("clampScale(%g) = %g, want %g", c.in, got, c.want)
		}
	}
}

func TestImageDims(t *testing.T) {
	// PNG: signature(8) + length(4) + "IHDR" + width(BE) + height(BE).
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	png = append(png, 0, 0, 0, 13, 'I', 'H', 'D', 'R')
	png = binary.BigEndian.AppendUint32(png, 1234)
	png = binary.BigEndian.AppendUint32(png, 5678)
	if w, h, ok := imageDims("png", png); !ok || w != 1234 || h != 5678 {
		t.Errorf("png dims = %d,%d,%v want 1234,5678,true", w, h, ok)
	}

	// JPEG: SOI then SOF0 carrying height then width (big-endian).
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x0D, 0x08, 0x02, 0x58, 0x03, 0x20, 0x00}
	if w, h, ok := imageDims("jpeg", jpg); !ok || w != 800 || h != 600 {
		t.Errorf("jpeg dims = %d,%d,%v want 800,600,true", w, h, ok)
	}

	// WebP VP8X (extended): canvas w-1/h-1 are 24-bit LE.
	vp8x := webpChunk("VP8X", []byte{0, 0, 0, 0, 0xFF, 0x03, 0x00, 0xFF, 0x02, 0x00})
	if w, h, ok := imageDims("webp", vp8x); !ok || w != 1024 || h != 768 {
		t.Errorf("vp8x dims = %d,%d,%v want 1024,768,true", w, h, ok)
	}

	// WebP VP8L (lossless): signature 0x2f then packed 14-bit w-1/h-1.
	vp8l := webpChunk("VP8L", []byte{0x2F, 0x63, 0x40, 0x0C, 0x00})
	if w, h, ok := imageDims("webp", vp8l); !ok || w != 100 || h != 50 {
		t.Errorf("vp8l dims = %d,%d,%v want 100,50,true", w, h, ok)
	}

	// WebP VP8 (lossy): sync 9d 01 2a then 14-bit w/h little-endian.
	vp8 := webpChunk("VP8 ", []byte{0, 0, 0, 0x9D, 0x01, 0x2A, 0x80, 0x02, 0xE0, 0x01})
	if w, h, ok := imageDims("webp", vp8); !ok || w != 640 || h != 480 {
		t.Errorf("vp8 dims = %d,%d,%v want 640,480,true", w, h, ok)
	}

	// Unknown/short headers must not report dimensions.
	if _, _, ok := imageDims("png", []byte{0x89, 'P'}); ok {
		t.Errorf("short png should not report dims")
	}
	if _, _, ok := imageDims("bmp", []byte("BMxxxxxxxx")); ok {
		t.Errorf("unsupported format should not report dims")
	}
}

// webpChunk builds a minimal RIFF/WEBP container with a single chunk.
func webpChunk(fourcc string, payload []byte) []byte {
	b := []byte("RIFF")
	b = append(b, 0, 0, 0, 0)
	b = append(b, 'W', 'E', 'B', 'P')
	b = append(b, fourcc...)
	b = append(b, 0, 0, 0, 0)
	return append(b, payload...)
}

func TestScreenshotMeta(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 'I', 'H', 'D', 'R'}
	png = binary.BigEndian.AppendUint32(png, 640)
	png = binary.BigEndian.AppendUint32(png, 480)

	m := screenshotMeta("region", "10,20,300,400", 1, true, "png", 111, png)
	for _, want := range []string{"mode=region", "clip=10,20,300,400", "size=640x480", "format=png", "bytes=111"} {
		if !strings.Contains(m, want) {
			t.Errorf("region meta %q missing %q", m, want)
		}
	}
	if strings.Contains(m, "scale=") {
		t.Errorf("scale should be omitted when it is 1: %q", m)
	}

	m2 := screenshotMeta("viewport", "", 0.5, true, "png", 42, png)
	for _, want := range []string{"mode=viewport", "clip=viewport", "scale=0.5", "size=640x480"} {
		if !strings.Contains(m2, want) {
			t.Errorf("viewport-scale meta %q missing %q", m2, want)
		}
	}

	m3 := screenshotMeta("viewport", "", 1, false, "png", 42, png)
	if strings.Contains(m3, "clip=") {
		t.Errorf("plain viewport capture should not report a clip: %q", m3)
	}
}
