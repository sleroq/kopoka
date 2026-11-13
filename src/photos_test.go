package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestMergeArtPhotosDownloadFailure(t *testing.T) {
	for _, body := range []string{"bad image", ""} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if body == "" {
				w.WriteHeader(http.StatusForbidden)
			}
			_, _ = w.Write([]byte(body))
		}))
		photos := []any{
			tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(server.URL)),
			tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(server.URL)),
		}
		got := mergeArtPhotos(photos)
		if !reflect.DeepEqual(got, photos) {
			t.Fatal("failed download changed album")
		}
		server.Close()
	}
}

func TestMergeArtPhotosDownloadAndMetadata(t *testing.T) {
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	var encoded [2]bytes.Buffer
	for i := range encoded {
		panel := scene.SubImage(image.Rect(i*200, 0, (i+1)*200, 320))
		err := png.Encode(&encoded[i], panel)
		if err != nil {
			t.Fatal(err)
		}
	}

	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		i, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || i < 0 || i > 1 {
			t.Errorf("unexpected photo request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(encoded[i].Bytes())
	}))
	defer server.Close()
	first := tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(server.URL + "/0"))
	first.Caption = "caption"
	first.ParseMode = "HTML"
	photos := []any{first, tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(server.URL + "/1"))}

	got := mergeArtPhotos(photos)
	if count != 2 || len(got) != 1 {
		t.Fatalf("downloads = %d, resulting photos = %d; want 2 and 1", count, len(got))
	}
	photo := got[0].(tgbotapi.InputMediaPhoto)
	if photo.Caption != first.Caption || photo.ParseMode != first.ParseMode {
		t.Fatal("lost metadata")
	}
	file, ok := photo.Media.(tgbotapi.FileBytes)
	if !ok || file.Name != "joined-art.jpg" {
		t.Fatalf("merged photo is not a JPEG upload: %T", photo.Media)
	}
	decoded, format, err := image.Decode(bytes.NewReader(file.Bytes))
	if err != nil || format != "jpeg" {
		t.Fatalf("invalid JPEG: %s %v", format, err)
	}
	if decoded.Bounds() != scene.Bounds() {
		t.Fatalf("merged bounds = %v, want %v", decoded.Bounds(), scene.Bounds())
	}

	// Pixiv images are already downloaded for upload before merging.
	first.Media = tgbotapi.FileBytes{Name: "0.png", Bytes: encoded[0].Bytes()}
	uploads := []any{
		first,
		tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "1.png", Bytes: encoded[1].Bytes()}),
	}
	got = mergeArtPhotos(uploads)
	if len(got) != 1 || count != 2 {
		t.Fatalf("uploaded panels were not merged locally: photos = %d, downloads = %d", len(got), count)
	}
	if !reflect.DeepEqual(got[0], photo) {
		t.Fatal("merging uploaded panels differed from merging URL panels")
	}
}

func TestMergeArtPhotosRetainsOriginalURLs(t *testing.T) {
	var encoded bytes.Buffer
	err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 200, 320)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(encoded.Bytes())
	}))
	defer server.Close()
	photos := []any{
		tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(server.URL + "/0")),
		tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(server.URL + "/1")),
	}
	got := mergeArtPhotos(photos)
	if !reflect.DeepEqual(got, photos) {
		t.Fatal("unmatched pictures were reencoded or reordered")
	}
}

func TestMergeArtPhotosLargeTriptych(t *testing.T) {
	// Source: https://x.com/emyo27/status/2116464003270779205.
	// Only the outer three columns of each original JPEG are retained.
	file, err := os.Open("testdata/emyo27-triptych-edges.png")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	edges, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}

	var photos []any
	for i := range 3 {
		panel := image.NewRGBA(image.Rect(0, 0, 2247, 4096))
		draw.Draw(panel, image.Rect(0, 0, 3, 4096), edges, image.Pt(i*6, 0), draw.Src)
		draw.Draw(panel, image.Rect(2244, 0, 2247, 4096), edges, image.Pt(i*6+3, 0), draw.Src)
		var data bytes.Buffer
		if err := png.Encode(&data, panel); err != nil {
			t.Fatal(err)
		}
		photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "panel.png", Bytes: data.Bytes()})
		if i == 0 {
			photo.Caption = "EMyo/えみょ"
			photo.ParseMode = "HTML"
		}
		photos = append(photos, photo)
	}

	got := mergeArtPhotos(photos)
	if len(got) != 1 {
		t.Fatalf("got %d photos, want all three panels merged into one", len(got))
	}
	photo := got[0].(tgbotapi.InputMediaPhoto)
	if photo.Caption != "EMyo/えみょ" || photo.ParseMode != "HTML" {
		t.Fatal("lost artwork caption")
	}
	upload := photo.Media.(tgbotapi.FileBytes)
	config, format, err := image.DecodeConfig(bytes.NewReader(upload.Bytes))
	if err != nil || format != "jpeg" {
		t.Fatalf("invalid JPEG upload: %s %v", format, err)
	}
	// 6741x4096 exceeds Telegram's width+height limit; fit the whole image.
	if config.Width != 6220 || config.Height != 3779 {
		t.Fatalf("upload is %dx%d, want 6220x3779", config.Width, config.Height)
	}
	if len(upload.Bytes) > 10*1024*1024 {
		t.Fatal("merged photo exceeds Telegram's upload limit")
	}
}

func TestSendPhotosBatches(t *testing.T) {
	for _, size := range []int{0, 1, 2, 10, 11, 21} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				w.Header().Set("Content-Type", "application/json")
				if method == "getMe" {
					_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test"}}`))
					return
				}

				methods = append(methods, method)
				err := r.ParseMultipartForm(1 << 20)
				if err != nil {
					t.Error(err)
				}
				if r.FormValue("chat_id") != "42" || r.FormValue("reply_to_message_id") != "7" {
					t.Error("lost destination")
				}
				if method == "sendPhoto" {
					if r.FormValue("caption") != "caption" || r.FormValue("parse_mode") != "HTML" {
						t.Error("lost singleton caption")
					}
					file, header, err := r.FormFile("photo")
					if err != nil {
						t.Error(err)
					} else {
						err = file.Close()
						if err != nil {
							t.Error(err)
						}
						if header.Filename != "joined-art.jpg" {
							t.Error(header.Filename)
						}
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
				} else {
					var media []struct {
						Caption   string `json:"caption"`
						ParseMode string `json:"parse_mode"`
					}
					err := json.Unmarshal([]byte(r.FormValue("media")), &media)
					if err != nil {
						t.Error(err)
					}
					if len(media) != min(size, 10) {
						t.Errorf("batch size %d", len(media))
					}
					for i, p := range media {
						if i == 0 {
							if p.Caption != "caption" || p.ParseMode != "HTML" {
								t.Error("lost batch caption")
							}
						} else if p.Caption != "" || p.ParseMode != "" {
							t.Error("stale caption")
						}
					}
					_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
				}
			}))
			defer server.Close()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}

			photos := make([]any, size)
			for i := range photos {
				p := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "joined-art.jpg", Bytes: []byte("jpeg")})
				p.Caption = "stale"
				p.ParseMode = "Markdown"
				if i == 0 {
					p.Caption = "caption"
					p.ParseMode = "HTML"
				}
				photos[i] = p
			}
			err = sendPhotos(bot, 42, 7, photos)
			if err != nil {
				t.Fatal(err)
			}

			var want []string
			for n := size; n > 0; n -= 10 {
				if n == 1 {
					want = append(want, "sendPhoto")
				} else {
					want = append(want, "sendMediaGroup")
				}
			}
			if !reflect.DeepEqual(methods, want) {
				t.Fatalf("methods %v, want %v", methods, want)
			}
		})
	}
}
