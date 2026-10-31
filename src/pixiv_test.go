package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestDownloadPixivPhotoUsesLargeVariant(t *testing.T) {
	var original bytes.Buffer
	bounds := image.Rect(0, 0, 200, 320)
	err := png.Encode(&original, image.NewRGBA(bounds))
	if err != nil {
		t.Fatal(err)
	}
	// Trailing padding makes a small, valid image exceed Telegram's upload limit.
	data := append(original.Bytes(), make([]byte, 10*1024*1024)...)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		if r.URL.Path == "/original.png" {
			_, _ = w.Write(data)
		} else {
			_, _ = w.Write(original.Bytes())
		}
	}))
	t.Cleanup(server.Close)

	file, err := downloadPixivPhoto(server.Client(), server.URL+"/original.png", server.URL+"/large.png")
	if err != nil {
		t.Fatal(err)
	}
	if file.Name != "large.png" || !bytes.Equal(file.Bytes, original.Bytes()) {
		t.Fatalf("Pixiv large variant was not used unchanged: %s, %d bytes", file.Name, len(file.Bytes))
	}
	if strings.Join(requests, ",") != "/original.png,/large.png" {
		t.Fatalf("requests = %v, want original then large", requests)
	}
}

func TestDownloadPixivPhotoNoSuitableVariant(t *testing.T) {
	data := make([]byte, 10*1024*1024+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)

	for _, largeURL := range []string{"", server.URL + "/large.jpg"} {
		_, err := downloadPixivPhoto(server.Client(), server.URL+"/original.jpg", largeURL)
		if err == nil {
			t.Fatal("expected an error when no variant fits the photo size limit")
		}
	}
}

func TestDownloadPixivPhotoHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	_, err := downloadPixivPhoto(server.Client(), server.URL, "")
	if err == nil {
		t.Fatal("expected a download error for HTTP 403")
	}
}

func TestSetupPixivClient(t *testing.T) {
	var original bytes.Buffer
	err := png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 200, 320)))
	if err != nil {
		t.Fatal(err)
	}
	pages, downloads := 1, 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "oauth.secure.pixiv.net" || r.Host == "app-api.pixiv.net" {
			if r.ProtoMajor != 1 {
				http.Error(w, "<html>Forbidden</html>", http.StatusForbidden)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/token":
			err := r.ParseForm()
			if err != nil {
				t.Error(err)
			}
			if r.Method != http.MethodPost || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "test-refresh" {
				t.Error("unexpected token refresh request")
			}
			_, err = io.WriteString(w, `{
				"response": {
					"access_token": "refreshed-access",
					"refresh_token": "test-refresh",
					"expires_in": 3600,
					"user": {}
				}
			}`)
			if err != nil {
				t.Error(err)
			}
		case "/v1/illust/detail":
			if r.Header.Get("Authorization") != "Bearer refreshed-access" {
				t.Error("illustration request did not use refreshed token")
			}
			body := `{"illust":{
				"id":123,
				"user":{"name":"artist"},
				"image_urls":{"large":"https://i.pximg.net/large.png"},
				"meta_single_page":{"original_image_url":"https://i.pximg.net/original.png"}
			}}`
			if pages == 2 {
				body = `{"illust":{
					"id":123,
					"user":{"name":"artist"},
					"meta_pages":[
						{"image_urls":{
							"original":"https://i.pximg.net/original.png",
							"large":"https://i.pximg.net/large.png"
						}},
						{"image_urls":{
							"original":"https://i.pximg.net/original.png",
							"large":"https://i.pximg.net/large.png"
						}}
					]
				}}`
			}
			_, err := io.WriteString(w, body)
			if err != nil {
				t.Error(err)
			}
		case "/original.png":
			if r.Referer() != "https://www.pixiv.net/" {
				http.Error(w, "missing Pixiv referer", http.StatusForbidden)
				return
			}
			downloads++
			_, _ = w.Write(original.Bytes())
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	base := server.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig.ServerName = "127.0.0.1"
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(base.CloseIdleConnections)

	originalClient, originalTransport := http.DefaultClient, http.DefaultTransport
	http.DefaultClient = &http.Client{}
	http.DefaultTransport = base
	t.Cleanup(func() {
		http.DefaultClient.Transport.(*pixivTransport).pixiv.CloseIdleConnections()
		http.DefaultClient, http.DefaultTransport = originalClient, originalTransport
	})

	t.Setenv("PIXIV_ACCESS_TOKEN", "old-access")
	t.Setenv("PIXIV_REFRESH_TOKEN", "test-refresh")
	t.Setenv("PIXIV_TOKEN_DEADLINE", "3600")

	app, err := setupPixivClient()
	if err != nil {
		t.Fatal(err)
	}
	illust, err := app.IllustDetail(123)
	if err != nil {
		t.Fatal(err)
	}
	if illust.ID != 123 {
		t.Errorf("illustration ID = %d, want 123", illust.ID)
	}

	link, err := url.Parse("https://www.pixiv.net/artworks/123")
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 2} {
		pages, downloads = count, 0
		photos, err := handlePixivLink(app, link)
		if err != nil {
			t.Fatal(err)
		}
		if len(photos) != count || downloads != count {
			t.Fatalf("photos = %d, downloads = %d; want %d each", len(photos), downloads, count)
		}
		for _, media := range photos {
			photo := media.(tgbotapi.InputMediaPhoto)
			file, ok := photo.Media.(tgbotapi.FileBytes)
			if !ok || file.Name != "original.png" || !bytes.Equal(file.Bytes, original.Bytes()) {
				t.Fatalf("Pixiv original was not preserved as an upload: %T", photo.Media)
			}
		}
		photo := photos[0].(tgbotapi.InputMediaPhoto)
		if photo.ParseMode != "HTML" || !strings.Contains(photo.Caption, "artist") {
			t.Fatal("lost Pixiv caption")
		}
	}

	for _, host := range []string{"api.telegram.org", "fixupx.com"} {
		resp, err := http.DefaultClient.Get("https://" + host + "/")
		if err != nil {
			t.Fatal(err)
		}
		err = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.ProtoMajor != 2 {
			t.Errorf("%s used %s, want HTTP/2", host, resp.Proto)
		}
	}
}
