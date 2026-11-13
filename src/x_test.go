package main

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type xPostTransport func(*http.Request) (*http.Response, error)

func (f xPostTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHandleXLink(t *testing.T) {
	var images []string
	for i := range 12 {
		images = append(images, fmt.Sprintf(`{"url":"https://example.com/%d.jpg"}`, i))
	}

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
		count   int
	}{
		{"ordered photos", 200, `{"code":200,"tweet":{"author":{"name":"ぽん & friends"},"media":{"photos":[` + strings.Join(images, ",") + `]}}}`, false, 12},
		{"no photos", 200, `{"code":200,"tweet":{"author":{"name":"ぽん"}}}`, false, 0},
		{"http error", 404, `{}`, true, 0},
		{"api error", 200, `{"code":404,"message":"not found"}`, true, 0},
		{"malformed json", 200, `{`, true, 0},
		{"missing tweet", 200, `{"code":200}`, true, 0},
		{"missing author", 200, `{"code":200,"tweet":{}}`, true, 0},
		{"missing photo url", 200, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"photos":[{}]}}}`, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := http.DefaultTransport
			http.DefaultTransport = xPostTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://api.fxtwitter.com/user/status/123" {
					t.Errorf("unexpected request: %s", r.URL)
				}
				return &http.Response{
					StatusCode: tt.status,
					Status:     http.StatusText(tt.status),
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Request:    r,
				}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			link, _ := url.Parse("https://x.com/user/status/123")
			photos, err := handleXLink(link)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error %v", err, tt.wantErr)
			}
			if len(photos) != tt.count {
				t.Fatalf("got %d photos, want %d", len(photos), tt.count)
			}
			for i, media := range photos {
				photo := media.(tgbotapi.InputMediaPhoto)
				if want := tgbotapi.FileURL(fmt.Sprintf("https://example.com/%d.jpg", i)); photo.Media != want {
					t.Errorf("photo %d = %v, want %v", i, photo.Media, want)
				}
				if i%10 == 0 {
					if photo.Caption != "<a href=\"https://fxtwitter.com/user/status/123\">ぽん &amp; friends</a>\n" || photo.ParseMode != "HTML" {
						t.Errorf("photo %d caption: %+v", i, photo.BaseInputMedia)
					}
				} else if photo.Caption != "" || photo.ParseMode != "" {
					t.Errorf("unexpected caption on photo %d", i)
				}
			}
		})
	}
}

func TestXPostLiveMerge(t *testing.T) {
	if testing.Short() || os.Getenv("KOPOKA_LIVE_TESTS") != "1" {
		t.Skip("live X API regression; set KOPOKA_LIVE_TESTS=1 to run")
	}
	link, _ := url.Parse("https://x.com/Cookiaria/status/2120987799931265207")
	photos, err := handleXLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 20 {
		t.Fatalf("got %d input photos, want 20", len(photos))
	}
	merged := mergeArtPhotos(photos)
	if len(merged) != 1 {
		t.Fatalf("got %d merged photos, want 1", len(merged))
	}
	photo := merged[0].(tgbotapi.InputMediaPhoto)
	file, ok := photo.Media.(tgbotapi.FileBytes)
	if !ok {
		t.Fatalf("merged media is %T, want FileBytes", photo.Media)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(file.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 4096 || cfg.Height != 2731 {
		t.Errorf("dimensions = %dx%d, want 4096x2731", cfg.Width, cfg.Height)
	}
}
