package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func artFixturePhotos(t *testing.T, fixture string, width, height, count int) []any {
	t.Helper()
	file, err := os.Open("testdata/" + fixture + "-edges.png")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	edges, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	photos := make([]any, count)
	for i := range photos {
		panel := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.Draw(panel, panel.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		draw.Draw(panel, image.Rect(0, 0, 3, height), edges, image.Pt(i*6, 0), draw.Src)
		draw.Draw(panel, image.Rect(width-3, 0, width, height), edges, image.Pt(i*6+3, 0), draw.Src)
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, panel); err != nil {
			t.Fatal(err)
		}
		photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: fmt.Sprintf("panel-%d.png", i), Bytes: encoded.Bytes()})
		photo.Caption = fmt.Sprintf("panel %d", i)
		photos[i] = photo
	}
	return photos
}

func checkMergedArt(t *testing.T, photos []any, sizes []image.Point, captions []int) {
	t.Helper()
	got := mergeArtPhotos(photos)
	if len(got) != len(sizes) {
		t.Fatalf("got %d photos, want %d", len(got), len(sizes))
	}
	for i, item := range got {
		photo := item.(tgbotapi.InputMediaPhoto)
		if want := fmt.Sprintf("panel %d", captions[i]); photo.Caption != want {
			t.Errorf("photo %d caption = %q, want %q", i, photo.Caption, want)
		}
		data := photo.Media.(tgbotapi.FileBytes)
		decoded, _, err := image.DecodeConfig(bytes.NewReader(data.Bytes))
		if err != nil {
			t.Fatal(err)
		}
		size := sizes[i]
		if total := size.X + size.Y; total > 10000 {
			size = image.Pt(size.X*10000/total, size.Y*10000/total)
		}
		if got := image.Pt(decoded.Width, decoded.Height); got != size {
			t.Errorf("photo %d size = %v, want %v", i, got, size)
		}
	}
}

func TestMergeArtPhotosReportedPosts(t *testing.T) {
	tests := []struct {
		name                 string
		width, height, count int
		sizes                []image.Point
		captions             []int
	}{
		// https://x.com/wenhaocha1/status/2121769943221891481
		{"wenhao-chart", 307, 4096, 20, []image.Point{{6140, 4096}}, []int{0}},
		{"wenhao-chart-large", 154, 2048, 20, []image.Point{{3080, 2048}}, []int{0}},
		// https://x.com/k1phex/status/2121351590250418434
		{"k1phex-scenes", 240, 1080, 12, []image.Point{{1440, 1080}, {1440, 1080}}, []int{0, 6}},
		// https://x.com/reyforn_/status/2121768052748349807
		{"reyforn-panorama", 1080, 1440, 20, []image.Point{{21600, 1440}}, []int{0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			photos := artFixturePhotos(t, tt.name, tt.width, tt.height, tt.count)
			checkMergedArt(t, photos, tt.sizes, tt.captions)
			if tt.name == "reyforn-panorama" {
				checkMergedArt(t, photos[1:4], []image.Point{{1080, 1440}, {1080, 1440}, {1080, 1440}}, []int{1, 2, 3})
				checkMergedArt(t, photos[:4], []image.Point{{2160, 1440}, {1080, 1440}, {1080, 1440}}, []int{0, 2, 3})
			}
		})
	}
}
