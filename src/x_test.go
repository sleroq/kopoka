package main

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const xMultipleImagesHTML = `<meta property="twitter:image" content="https://mosaic.fxtwitter.com/jpeg/2103104016725016856/HS-58ZWaoAAA7vb/HS-58p2bAAA_lS1/HS-589IaUAAkbgB/HS-59PUasAAKIpk"/>
<link rel="alternate" href="https://fixupx.com/owoembed?status=2103104016725016856" type="application/json+oembed" title="ぽん">`

func TestMatchXImageLinks(t *testing.T) {
	const postPath = "/valhalla0707/status/2103104016725016856"
	const photoURL = "https://d.fixupx.com" + postPath + "/photo/"

	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "four images from reported post",
			body: xMultipleImagesHTML,
			want: []string{photoURL + "1", photoURL + "2", photoURL + "3", photoURL + "4"},
		},
		{
			name: "two images with hyphens and underscores",
			body: `<meta property="twitter:image" content="https://mosaic.fxtwitter.com/jpeg/123/first_image/second-image"/>`,
			want: []string{photoURL + "1", photoURL + "2"},
		},
		{
			name: "three images with a hyphen in the middle ID",
			body: `<meta property="twitter:image" content="https://mosaic.fxtwitter.com/jpeg/123/first/second-image/third"/>`,
			want: []string{photoURL + "1", photoURL + "2", photoURL + "3"},
		},
		{
			name: "existing mosaic IDs",
			body: `<meta property="twitter:image" content="https://mosaic.fxtwitter.com/jpeg/123/first_image/second_image"/>`,
			want: []string{photoURL + "1", photoURL + "2"},
		},
		{
			name: "single image",
			body: `<meta property="twitter:image" content="https://pbs.twimg.com/media/single-image.jpg"/>`,
			want: []string{"https://pbs.twimg.com/media/single-image.jpg"},
		},
		{
			name: "no images",
			body: `<html></html>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := matchXImageLinks(tt.body, postPath)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("image links = %v, want %v", got, tt.want)
			}
		})
	}
}

type xPostTransport func(*http.Request) (*http.Response, error)

func (f xPostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestHandleXLinkMultipleImages(t *testing.T) {
	originalTransport := http.DefaultTransport
	http.DefaultTransport = xPostTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://i.fixupx.com/valhalla0707/status/2103104016725016856" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(xMultipleImagesHTML)),
			Request:    r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	link, err := url.Parse("https://x.com/valhalla0707/status/2103104016725016856")
	if err != nil {
		t.Fatal(err)
	}
	photos, err := handleXLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 4 {
		t.Fatalf("got %d photos, want 4", len(photos))
	}
	for i, media := range photos {
		photo, ok := media.(tgbotapi.InputMediaPhoto)
		if !ok {
			t.Fatalf("photo %d has type %T, want InputMediaPhoto", i+1, media)
		}
		wantURL := tgbotapi.FileURL("https://d.fixupx.com/valhalla0707/status/2103104016725016856/photo/" + strconv.Itoa(i+1))
		if photo.Media != wantURL {
			t.Errorf("photo %d URL = %v, want %v", i+1, photo.Media, wantURL)
		}
		if i == 0 {
			if photo.Caption != "<a href=\"https://fxtwitter.com/valhalla0707/status/2103104016725016856\">ぽん</a>\n" || photo.ParseMode != "HTML" {
				t.Errorf("unexpected first photo caption: %+v", photo.BaseInputMedia)
			}
		} else if photo.Caption != "" {
			t.Errorf("photo %d has an unexpected caption: %q", i+1, photo.Caption)
		}
	}
}
