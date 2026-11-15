package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestPostImagePlaceholderHTTP(t *testing.T) {
	previews := testPreviews(t, testArtworkDB(t), nil)
	server := httptest.NewServer(previews)
	defer server.Close()

	for _, theme := range []string{"light", "warm", "dark", "dim"} {
		t.Run(theme, func(t *testing.T) {
			photo := previews.postImagePlaceholder(theme)
			response, err := http.Get(server.URL + "/post-image-placeholder-" + theme + ".jpg")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/jpeg" {
				t.Fatalf("response: %s, %s", response.Status, response.Header.Get("Content-Type"))
			}
			picture, err := jpeg.Decode(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if picture.Bounds().Dx() != photo.width || picture.Bounds().Dy() != photo.height {
				t.Fatalf("placeholder dimensions: %v", picture.Bounds())
			}
			red, _, _, _ := picture.At(0, 0).RGBA()
			if theme == "light" && red < 65000 || theme == "dark" && red > 500 {
				t.Fatalf("%s placeholder background = %d", theme, red)
			}
		})
	}
}

func TestThemePreviewHTTP(t *testing.T) {
	t.Setenv("KOPOKA_RENDERER", testPostImageRenderer(t))
	previews := testPreviews(t, testArtworkDB(t), nil)

	for _, sample := range []struct {
		theme      string
		background [3]uint32
	}{
		{"light", [3]uint32{247, 249, 249}},
		{"warm", [3]uint32{255, 246, 245}},
		{"dark", [3]uint32{0, 0, 0}},
		{"dim", [3]uint32{22, 24, 28}},
	} {
		var photos, thumbnails [][]byte
		for _, selected := range []bool{false, true} {
			photo, err := previews.themePreview(sample.theme, selected)
			if err != nil {
				t.Fatal(err)
			}
			if photo.photoURL == photo.thumbnailURL || photo.width < 1000 || photo.width == photo.height {
				t.Fatalf("%s photo: %+v", sample.theme, photo)
			}
			photoData, photoSize := themePreviewResponse(t, previews, photo.photoURL, sample.background)
			thumbnailData, thumbnailSize := themePreviewResponse(t, previews, photo.thumbnailURL, sample.background)
			photos = append(photos, photoData)
			thumbnails = append(thumbnails, thumbnailData)
			if photoSize != image.Pt(photo.width, photo.height) || max(thumbnailSize.X, thumbnailSize.Y) != 384 ||
				math.Abs(float64(thumbnailSize.X*photo.height-thumbnailSize.Y*photo.width)) > float64(max(photo.width, photo.height)) {
				t.Fatalf("%s dimensions: %v, %v", sample.theme, photoSize, thumbnailSize)
			}
		}
		if !bytes.Equal(photos[0], photos[1]) || bytes.Equal(thumbnails[0], thumbnails[1]) {
			t.Fatalf("%s selected badge missing from thumbnail or present on photo", sample.theme)
		}
	}

	// The generated previews are reused without another renderer invocation.
	t.Setenv("KOPOKA_RENDERER", filepath.Join(t.TempDir(), "missing"))
	photo, err := previews.themePreview("warm", true)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	previews.ServeHTTP(response, httptest.NewRequest("GET", photo.photoURL, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("cached preview: %d", response.Code)
	}
	response = httptest.NewRecorder()
	previews.ServeHTTP(response, httptest.NewRequest("GET", "/theme-preview?theme=invalid", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid theme: %d", response.Code)
	}
}

func themePreviewResponse(t *testing.T, previews *thumbnails, address string, background [3]uint32) ([]byte, image.Point) {
	t.Helper()
	response := httptest.NewRecorder()
	previews.ServeHTTP(response, httptest.NewRequest("GET", address, nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("%s: %d %s", address, response.Code, response.Body.String())
	}
	picture, err := jpeg.Decode(bytes.NewReader(response.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	red, green, blue, _ := picture.At(0, 0).RGBA()
	for i, value := range []uint32{red >> 8, green >> 8, blue >> 8} {
		if value+3 < background[i] || value > background[i]+3 {
			t.Fatalf("%s background: %d,%d,%d", address, red>>8, green>>8, blue>>8)
		}
	}
	return response.Body.Bytes(), picture.Bounds().Size()
}
