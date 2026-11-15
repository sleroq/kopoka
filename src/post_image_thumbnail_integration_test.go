package main

import (
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostImagePlaceholderHTTP(t *testing.T) {
	previews := testPreviews(t, testArtworkDB(t), nil)
	server := httptest.NewServer(previews)
	defer server.Close()

	for _, theme := range []string{"light", "dark"} {
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
