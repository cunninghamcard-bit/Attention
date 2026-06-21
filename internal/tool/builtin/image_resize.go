package builtin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"slices"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	readImageMaxWidth       = 2000
	readImageMaxHeight      = 2000
	readImageMaxBase64Bytes = int(4.5 * 1024 * 1024)
	readImageJPEGQuality    = 80
)

type readImageResizeOptions struct {
	maxWidth       int
	maxHeight      int
	maxBase64Bytes int
	jpegQuality    int
}

type readImageResizeResult struct {
	Data           []byte
	MimeType       string
	OriginalWidth  int
	OriginalHeight int
	Width          int
	Height         int
	WasResized     bool
}

type encodedReadImageCandidate struct {
	Data     []byte
	MimeType string
}

func resizeReadImage(input []byte, mimeType string) *readImageResizeResult {
	return resizeReadImageWithOptions(input, mimeType, readImageResizeOptions{})
}

func resizeReadImageWithOptions(
	input []byte,
	mimeType string,
	opts readImageResizeOptions,
) *readImageResizeResult {
	opts = normalizeReadImageResizeOptions(opts)
	config, err := decodeReadImageConfig(input, mimeType)
	if err != nil {
		return nil
	}

	originalWidth := config.Width
	originalHeight := config.Height
	if originalWidth <= 0 || originalHeight <= 0 || opts.maxBase64Bytes <= 0 {
		return nil
	}

	if originalWidth <= opts.maxWidth &&
		originalHeight <= opts.maxHeight &&
		base64.StdEncoding.EncodedLen(len(input)) < opts.maxBase64Bytes {
		return &readImageResizeResult{
			Data:           input,
			MimeType:       mimeType,
			OriginalWidth:  originalWidth,
			OriginalHeight: originalHeight,
			Width:          originalWidth,
			Height:         originalHeight,
			WasResized:     false,
		}
	}

	source, err := decodeReadImage(input, mimeType)
	if err != nil {
		return nil
	}

	targetWidth, targetHeight := fitReadImageDimensions(
		originalWidth,
		originalHeight,
		opts.maxWidth,
		opts.maxHeight,
	)
	qualities := readImageJPEGQualitySteps(opts.jpegQuality)
	currentWidth := targetWidth
	currentHeight := targetHeight
	for {
		resized := resizeImageTo(source, currentWidth, currentHeight)
		candidates := encodeReadImageCandidates(resized, qualities)
		for _, candidate := range candidates {
			if base64.StdEncoding.EncodedLen(len(candidate.Data)) < opts.maxBase64Bytes {
				return &readImageResizeResult{
					Data:           candidate.Data,
					MimeType:       candidate.MimeType,
					OriginalWidth:  originalWidth,
					OriginalHeight: originalHeight,
					Width:          currentWidth,
					Height:         currentHeight,
					WasResized:     true,
				}
			}
		}

		if currentWidth == 1 && currentHeight == 1 {
			break
		}
		nextWidth := 1
		if currentWidth > 1 {
			nextWidth = max(1, int(math.Floor(float64(currentWidth)*0.75)))
		}
		nextHeight := 1
		if currentHeight > 1 {
			nextHeight = max(1, int(math.Floor(float64(currentHeight)*0.75)))
		}
		if nextWidth == currentWidth && nextHeight == currentHeight {
			break
		}
		currentWidth = nextWidth
		currentHeight = nextHeight
	}

	return nil
}

func normalizeReadImageResizeOptions(opts readImageResizeOptions) readImageResizeOptions {
	if opts.maxWidth == 0 {
		opts.maxWidth = readImageMaxWidth
	}
	if opts.maxHeight == 0 {
		opts.maxHeight = readImageMaxHeight
	}
	if opts.maxBase64Bytes == 0 {
		opts.maxBase64Bytes = readImageMaxBase64Bytes
	}
	if opts.jpegQuality == 0 {
		opts.jpegQuality = readImageJPEGQuality
	}
	return opts
}

func decodeReadImageConfig(input []byte, mimeType string) (image.Config, error) {
	reader := bytes.NewReader(input)
	switch mimeType {
	case "image/gif":
		return gif.DecodeConfig(reader)
	case "image/jpeg":
		return jpeg.DecodeConfig(reader)
	case "image/png":
		return png.DecodeConfig(reader)
	case "image/webp":
		return webp.DecodeConfig(reader)
	default:
		return image.Config{}, fmt.Errorf("unsupported image mime type %q", mimeType)
	}
}

func decodeReadImage(input []byte, mimeType string) (image.Image, error) {
	reader := bytes.NewReader(input)
	switch mimeType {
	case "image/gif":
		return gif.Decode(reader)
	case "image/jpeg":
		return jpeg.Decode(reader)
	case "image/png":
		return png.Decode(reader)
	case "image/webp":
		return webp.Decode(reader)
	default:
		return nil, fmt.Errorf("unsupported image mime type %q", mimeType)
	}
}

func fitReadImageDimensions(width, height, maxWidth, maxHeight int) (int, int) {
	targetWidth := width
	targetHeight := height
	if targetWidth > maxWidth {
		targetHeight = int(math.Round(float64(targetHeight) * float64(maxWidth) / float64(targetWidth)))
		targetWidth = maxWidth
	}
	if targetHeight > maxHeight {
		targetWidth = int(math.Round(float64(targetWidth) * float64(maxHeight) / float64(targetHeight)))
		targetHeight = maxHeight
	}
	return max(1, targetWidth), max(1, targetHeight)
}

func resizeImageTo(source image.Image, width, height int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), source, source.Bounds(), xdraw.Over, nil)
	return dst
}

func encodeReadImageCandidates(
	img image.Image,
	jpegQualities []int,
) []encodedReadImageCandidate {
	candidates := []encodedReadImageCandidate{}
	if data, err := encodeReadImagePNG(img); err == nil {
		candidates = append(candidates, encodedReadImageCandidate{
			Data:     data,
			MimeType: "image/png",
		})
	}
	for _, quality := range jpegQualities {
		if data, err := encodeReadImageJPEG(img, quality); err == nil {
			candidates = append(candidates, encodedReadImageCandidate{
				Data:     data,
				MimeType: "image/jpeg",
			})
		}
	}
	return candidates
}

func encodeReadImagePNG(img image.Image) ([]byte, error) {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func encodeReadImageJPEG(img image.Image, quality int) ([]byte, error) {
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func readImageJPEGQualitySteps(primary int) []int {
	steps := []int{}
	for _, quality := range []int{primary, 85, 70, 55, 40} {
		if quality <= 0 {
			continue
		}
		if quality > 100 {
			quality = 100
		}
		if !slices.Contains(steps, quality) {
			steps = append(steps, quality)
		}
	}
	return steps
}

func formatReadImageDimensionNote(result *readImageResizeResult) string {
	if result == nil || !result.WasResized {
		return ""
	}
	scale := float64(result.OriginalWidth) / float64(result.Width)
	return fmt.Sprintf(
		"[Image: original %dx%d, displayed at %dx%d. Multiply coordinates by %.2f to map to original image.]",
		result.OriginalWidth,
		result.OriginalHeight,
		result.Width,
		result.Height,
		scale,
	)
}
