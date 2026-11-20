package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"math"

	"golang.org/x/image/draw"
)

type previewBlock struct {
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Image []byte `json:"image,omitempty"`
	Video bool   `json:"video"`
	Count int    `json:"count"`
}

func (t *thumbnails) renderMessagePreview(ctx context.Context, blocks []previewBlock) (small, large []byte, width, height int, err error) {
	_, span := startInlineSpan(ctx, "preview.message.render")
	defer func() { inlineSpanError(span, err); span.End() }()
	var request struct {
		Preview struct {
			Bot    string         `json:"bot"`
			Blocks []previewBlock `json:"blocks"`
		} `json:"preview"`
	}
	if t.bot != nil {
		request.Preview.Bot = t.bot.Self.UserName
	}
	request.Preview.Blocks = blocks
	large, err = runRenderer(request)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	img, err := jpeg.Decode(bytes.NewReader(large))
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("invalid message preview: %w", err)
	}
	bounds := img.Bounds()
	width, height = bounds.Dx(), bounds.Dy()
	longest := max(width, height)
	smallWidth := max(1, int(math.Round(float64(width)*384/float64(longest))))
	smallHeight := max(1, int(math.Round(float64(height)*384/float64(longest))))
	thumbnail := image.NewRGBA(image.Rect(0, 0, smallWidth, smallHeight))
	draw.CatmullRom.Scale(thumbnail, thumbnail.Bounds(), img, bounds, draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, thumbnail, &jpeg.Options{Quality: 90}); err != nil {
		return nil, nil, 0, 0, err
	}
	return out.Bytes(), large, width, height, nil
}
