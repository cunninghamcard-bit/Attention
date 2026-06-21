package builtin

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestResizeReadImageReturnsNilWhenStillOverInlineLimit(t *testing.T) {
	t.Parallel()

	input := testPNGBytes(t, 1, 1)
	result := resizeReadImageWithOptions(input, "image/png", readImageResizeOptions{
		maxWidth:       2000,
		maxHeight:      2000,
		maxBase64Bytes: 1,
		jpegQuality:    80,
	})
	if result != nil {
		t.Fatalf("resizeReadImageWithOptions() = %#v, want nil", result)
	}
}

func TestFormatReadImageDimensionNote(t *testing.T) {
	t.Parallel()

	result := &readImageResizeResult{
		OriginalWidth:  2100,
		OriginalHeight: 100,
		Width:          2000,
		Height:         95,
		WasResized:     true,
	}
	got := formatReadImageDimensionNote(result)
	if !strings.Contains(got, "original 2100x100, displayed at 2000x95") {
		t.Fatalf("formatReadImageDimensionNote() = %q, want dimensions", got)
	}
	if !strings.Contains(got, "Multiply coordinates by 1.05") {
		t.Fatalf("formatReadImageDimensionNote() = %q, want scale", got)
	}

	result.WasResized = false
	if got := formatReadImageDimensionNote(result); got != "" {
		t.Fatalf("formatReadImageDimensionNote(not resized) = %q, want empty", got)
	}
}

func testPNGBytes(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: 0x66, G: 0x88, B: 0xaa, A: 0xff})
		}
	}

	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatalf("Encode PNG fixture: %v", err)
	}
	return buffer.Bytes()
}
