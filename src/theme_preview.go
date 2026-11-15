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

const themePreviewVersion = 2

type themePreviewImages struct {
	photo, thumbnail, selected []byte
	width, height              int
}

func (t *thumbnails) themePreview(theme string, selected bool) (previewPhoto, error) {
	images, err := t.renderThemePreview(theme)
	if err != nil {
		return previewPhoto{}, err
	}
	address := *t.base
	address.Path += "/theme-preview"
	query := url.Values{
		"theme": {theme}, "v": {strconv.Itoa(postImageVersion) + "-" + strconv.Itoa(themePreviewVersion)},
	}
	query.Set("size", "large")
	address.RawQuery = query.Encode()
	photoURL := address.String()
	query.Del("size")
	query.Set("selected", strconv.FormatBool(selected))
	address.RawQuery = query.Encode()
	return previewPhoto{
		photoURL: photoURL, thumbnailURL: address.String(),
		width: images.width, height: images.height,
	}, nil
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
	data := images.thumbnail
	if r.URL.Query().Get("size") == "large" {
		data = images.photo
	} else if r.URL.Query().Get("selected") == "true" {
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
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), card, bounds, draw.Src, nil)
	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return images, err
	}

	drawThemeSelectionBadge(canvas)
	var selected bytes.Buffer
	if err := jpeg.Encode(&selected, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return images, err
	}
	images = themePreviewImages{
		photo: data, thumbnail: plain.Bytes(), selected: selected.Bytes(),
		width: size.X, height: size.Y,
	}
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
	width, height := canvas.Bounds().Dx(), canvas.Bounds().Dy()
	shorter := min(width, height)
	badgeBounds := image.Rect(width-shorter, 0, width, 56*shorter/384)
	draw.CatmullRom.Scale(canvas, badgeBounds, badge, badge.Bounds(), draw.Over, nil)
}
