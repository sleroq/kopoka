package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"golang.org/x/image/draw"
	"golang.org/x/image/vector"
)

type themePreviewImages struct {
	plain, selected []byte
}

func (t *thumbnails) themePreview(theme string, selected bool) previewPhoto {
	address := *t.base
	address.Path += "/theme-preview"
	address.RawQuery = url.Values{
		"theme": {theme}, "selected": {strconv.FormatBool(selected)}, "v": {strconv.Itoa(postImageVersion)},
	}.Encode()
	return previewPhoto{photoURL: address.String(), thumbnailURL: address.String(), width: 384, height: 384}
}

func (t *thumbnails) serveThemePreview(w http.ResponseWriter, r *http.Request) {
	theme := r.URL.Query().Get("theme")
	if _, ok := themeOption(theme); !ok {
		http.Error(w, "invalid theme", http.StatusBadRequest)
		return
	}
	images, err := t.renderThemePreview(theme)
	if err != nil {
		log.Printf("rendering theme preview for %s: %v", theme, err)
		http.Error(w, "theme preview generation failed", http.StatusBadGateway)
		return
	}
	data := images.plain
	if r.URL.Query().Get("selected") == "true" {
		data = images.selected
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(data)
}

func (t *thumbnails) renderThemePreview(theme string) (themePreviewImages, error) {
	t.themePreviewMu.Lock()
	images, ok := t.themePreviews[theme]
	t.themePreviewMu.Unlock()
	if ok {
		return images, nil
	}

	request := renderRequest{Theme: theme, Post: renderPost{
		AuthorName: "Kopoka", AuthorUsername: "kopoka",
		Text:      "A little color goes a long way.\nChoose the look that feels right to you.",
		CreatedAt: "Nov 15, 2026", Replies: 28, Retweets: 103, Likes: 2048,
		Quote: &renderPost{AuthorName: "Your feed", AuthorUsername: "yourfeed", Text: "Made for the moments you want to share."},
	}}
	data, err := renderPostImage(request)
	if err != nil {
		return images, err
	}
	card, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return images, err
	}
	bounds := card.Bounds()
	size := bounds.Size()
	width, height := size.X*384/max(size.X, size.Y), size.Y*384/max(size.X, size.Y)
	canvas := image.NewRGBA(image.Rect(0, 0, 384, 384))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(card.At(bounds.Min.X, bounds.Min.Y)), image.Point{}, draw.Src)
	destination := image.Rect((384-width)/2, (384-height)/2, (384+width)/2, (384+height)/2)
	draw.CatmullRom.Scale(canvas, destination, card, bounds, draw.Src, nil)
	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return images, err
	}

	drawThemeSelectionBadge(canvas)
	var selected bytes.Buffer
	if err := jpeg.Encode(&selected, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return images, err
	}
	images = themePreviewImages{plain: plain.Bytes(), selected: selected.Bytes()}
	t.themePreviewMu.Lock()
	t.themePreviews[theme] = images
	t.themePreviewMu.Unlock()
	return images, nil
}

func drawThemeSelectionBadge(canvas *image.RGBA) {
	badge := image.NewRGBA(image.Rect(0, 0, 384*4, 56*4))
	badgeBackground(badge, 332*4, 370*4)
	check := vector.NewRasterizer(badge.Bounds().Dx(), badge.Bounds().Dy())
	check.MoveTo(340*4, 34*4)
	check.LineTo(346*4, 40*4)
	check.LineTo(360*4, 26*4)
	check.LineTo(363*4, 29*4)
	check.LineTo(346*4, 44*4)
	check.LineTo(337*4, 37*4)
	check.ClosePath()
	check.Draw(badge, badge.Bounds(), image.NewUniform(color.White), image.Point{})
	draw.CatmullRom.Scale(canvas, image.Rect(0, 0, 384, 56), badge, badge.Bounds(), draw.Over, nil)
}
