package main

import (
	"bytes"
	"image/jpeg"
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
		var pictures [][]byte
		for _, selected := range []bool{false, true} {
			photo := previews.themePreview(sample.theme, selected)
			response := httptest.NewRecorder()
			previews.ServeHTTP(response, httptest.NewRequest("GET", photo.photoURL, nil))
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/jpeg" {
				t.Fatalf("%s: %d %s", sample.theme, response.Code, response.Body.String())
			}
			picture, err := jpeg.Decode(bytes.NewReader(response.Body.Bytes()))
			if err != nil || picture.Bounds().Dx() != photo.width || picture.Bounds().Dy() != photo.height {
				t.Fatalf("%s: %v", sample.theme, err)
			}
			red, green, blue, _ := picture.At(0, 0).RGBA()
			for i, value := range []uint32{red >> 8, green >> 8, blue >> 8} {
				if value+3 < sample.background[i] || value > sample.background[i]+3 {
					t.Fatalf("%s background: %d,%d,%d", sample.theme, red>>8, green>>8, blue>>8)
				}
			}
			pictures = append(pictures, response.Body.Bytes())
		}
		if bytes.Equal(pictures[0], pictures[1]) {
			t.Fatalf("%s selected badge missing", sample.theme)
		}
	}

	// The generated previews are reused without another renderer invocation.
	t.Setenv("KOPOKA_RENDERER", filepath.Join(t.TempDir(), "missing"))
	response := httptest.NewRecorder()
	previews.ServeHTTP(response, httptest.NewRequest("GET", previews.themePreview("warm", true).photoURL, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("cached preview: %d", response.Code)
	}
	response = httptest.NewRecorder()
	previews.ServeHTTP(response, httptest.NewRequest("GET", "/theme-preview?theme=invalid", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid theme: %d", response.Code)
	}
}
