package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSendPhotosRetryNarrowImages(t *testing.T) {
	asset := image.NewRGBA(image.Rect(0, 0, 40, 1200))
	for y := range 1200 {
		for x := range 40 {
			asset.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	var data bytes.Buffer
	if err := jpeg.Encode(&data, asset, nil); err != nil {
		t.Fatal(err)
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data.Bytes())
	}))
	defer source.Close()

	calls := 0
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
			return
		}
		calls++
		if calls == 1 {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 400,
				"description": `Bad Request: failed to send message #2 with the error message "WEBPAGE_CURL_FAILED"`,
			})
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		for _, name := range []string{"file-0", "file-1"} {
			file, _, err := r.FormFile(name)
			if err != nil {
				t.Error(err)
				return
			}
			img, _, err := image.Decode(file)
			_ = file.Close()
			if err != nil || img.Bounds().Size() != image.Pt(60, 1200) {
				t.Errorf("uploaded image: %v %v", img, err)
			}
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[`+
			`{"message_id":1,"photo":[{"file_id":"one"}]},`+
			`{"message_id":2,"photo":[{"file_id":"two"}]}]}`)
	}))
	defer telegram.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(source.URL + "/image.jpg"))
	ids, err := sendPhotosWithIDs(bot, 42, 7, []any{photo, photo})
	if err != nil || !reflect.DeepEqual(ids, []string{"one", "two"}) || calls != 2 {
		t.Fatalf("ids=%v calls=%d error=%v", ids, calls, err)
	}
}

type videoRetryCase struct {
	name                                                         string
	mixed, prior, downloadFail, retryFail, otherError, curlError bool
}

func TestSendPhotosURLVideoRetry(t *testing.T) {
	for _, tc := range []videoRetryCase{
		{name: "single"}, {name: "mixed curl", mixed: true, curlError: true}, {name: "mixed", mixed: true}, {name: "prior batch", mixed: true, prior: true},
		{name: "download failure", downloadFail: true}, {name: "retry failure", retryFail: true}, {name: "other error", otherError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads := 0
			videoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads++
				if tc.downloadFail {
					http.Error(w, "unavailable", 500)
					return
				}
				_, _ = io.WriteString(w, "video bytes")
			}))
			defer videoServer.Close()

			videoURL := videoServer.URL + "/video.mp4"
			calls := 0
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveVideoRetryTelegram(t, tc, videoURL, &calls, w, r)
			}))
			defer telegram.Close()

			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			video := tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(videoURL))
			video.Caption = "caption"
			video.ParseMode = "HTML"
			photos := []any{video}

			if tc.mixed {
				photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL("https://example.com/photo.jpg"))
				photo.Caption = "caption"
				photo.ParseMode = "HTML"
				photos = []any{photo, video}
			}
			if tc.prior {
				first := photos[0]
				prefix := make([]any, 10)
				for i := range prefix {
					prefix[i] = first
				}
				photos = append(prefix, photos...)
			}

			original := append([]any(nil), photos...)
			ids, err := sendPhotosWithIDs(bot, 42, 7, photos)
			if !reflect.DeepEqual(photos, original) {
				t.Error("input media changed")
			}
			if tc.downloadFail || tc.retryFail || tc.otherError {
				if err == nil {
					t.Fatal("expected an error")
				}
				if tc.retryFail || tc.otherError {
					var telegramErr *tgbotapi.Error
					expected := "Bad Request: upload rejected"
					if tc.otherError {
						expected = "Bad Request: another error"
					}
					if !errors.As(err, &telegramErr) || telegramErr.Message != expected {
						t.Fatalf("error = %v, want %s", err, expected)
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(ids) == 0 || ids[len(ids)-1] != "video-id" {
					t.Errorf("ids: %v", ids)
				}
			}

			wantCalls := 2
			if tc.prior {
				wantCalls++
			}
			if tc.downloadFail || tc.otherError {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Errorf("calls = %d, want %d", calls, wantCalls)
			}
			wantDownloads := 1
			if tc.otherError {
				wantDownloads = 0
			}
			if downloads != wantDownloads {
				t.Errorf("downloads = %d, want %d", downloads, wantDownloads)
			}
		})
	}
}

func serveVideoRetryTelegram(t *testing.T, tc videoRetryCase, videoURL string, calls *int, w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/getMe") {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
		return
	}

	*calls++
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	} else if err := r.ParseForm(); err != nil {
		t.Error(err)
		return
	}
	if r.FormValue("chat_id") != "42" || r.FormValue("reply_to_message_id") != "7" {
		t.Errorf("routing: %v", r.Form)
	}

	prior := tc.prior && *calls == 1
	retry := r.MultipartForm != nil && len(r.MultipartForm.File) > 0
	if !prior {
		if tc.mixed {
			var media []struct {
				Type      string `json:"type"`
				Media     string `json:"media"`
				Caption   string `json:"caption"`
				ParseMode string `json:"parse_mode"`
			}
			if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
				t.Error(err)
				return
			}
			expectedVideo := videoURL
			if retry {
				expectedVideo = "attach://file-1"
			}
			if len(media) != 2 || media[0].Type != "photo" || media[0].Media != "https://example.com/photo.jpg" || media[0].Caption != "caption" || media[0].ParseMode != "HTML" || media[1].Type != "video" || media[1].Media != expectedVideo || media[1].Caption != "" {
				t.Errorf("media: %+v", media)
			}
		} else if r.FormValue("caption") != "caption" || r.FormValue("parse_mode") != "HTML" || (!retry && r.FormValue("video") != videoURL) {
			t.Errorf("video form: %v", r.Form)
		}
	}

	if retry {
		name := "video"
		if tc.mixed {
			name = "file-1"
		}
		file, _, err := r.FormFile(name)
		if err != nil {
			t.Error(err)
			return
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || string(data) != "video bytes" {
			t.Errorf("upload: %q %v", data, err)
		}
	}

	if !prior && (!retry || tc.retryFail) {
		message := "Bad Request: failed to get HTTP URL content"
		if tc.curlError {
			message = "WEBPAGE_CURL_FAILED"
		}
		if tc.otherError {
			message = "Bad Request: another error"
		}
		if retry {
			message = "Bad Request: upload rejected"
		}
		w.WriteHeader(400)
		_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":400,"description":%q}`, message)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/sendMediaGroup") {
		_, _ = io.WriteString(w, `{"ok":true,"result":[`+
			`{"message_id":1,"photo":[{"file_id":"photo-id"}]},`+
			`{"message_id":2,"video":{"file_id":"video-id"}}]}`)
	} else {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":2,"video":{"file_id":"video-id"}}}`)
	}
}

func TestMixedUploadedPhotoVideoURLRetry(t *testing.T) {
	for _, unrelated := range []bool{false, true} {
		t.Run(fmt.Sprintf("unrelated=%t", unrelated), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)

			var photo bytes.Buffer
			if err := jpeg.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 10, 10)), nil); err != nil {
				t.Fatal(err)
			}
			downloads := []string{}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads = append(downloads, r.URL.Path)
				if r.URL.Path == "/video" {
					_, _ = io.WriteString(w, "video")
					return
				}
				_, _ = w.Write(photo.Bytes())
			}))
			defer source.Close()
			calls := 0
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/getMe") {
					_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
					return
				}
				calls++
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = r.MultipartForm.RemoveAll() }()
				var media []struct {
					Media   string `json:"media"`
					Caption string `json:"caption"`
				}
				if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
					t.Error(err)
					return
				}
				want := []string{"attach://file-0", source.URL + "/video", source.URL + "/photo"}
				if calls >= 2 {
					want[1] = "attach://file-1"
				}
				if calls >= 3 {
					want[2] = "attach://file-2"
				}
				if len(media) != 3 {
					t.Errorf("media: %+v", media)
					return
				}
				for i := range media {
					if media[i].Media != want[i] {
						t.Errorf("media %d: %s want %s", i, media[i].Media, want[i])
					}
				}
				if media[0].Caption != "caption" {
					t.Errorf("caption: %+v", media)
				}
				if calls <= 2 {
					message := "WEBPAGE_CURL_FAILED"
					if unrelated {
						message = "Bad Request: unrelated"
					}
					w.WriteHeader(400)
					_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":400,"description":%q}`, message)
					return
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":[`+
					`{"photo":[{"file_id":"p1"}]},{"video":{"file_id":"v"}},{"photo":[{"file_id":"p2"}]}]}`)
			}))
			defer telegram.Close()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			uploaded := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "photo.jpg", Bytes: photo.Bytes()})
			uploaded.Caption = "caption"
			batch := []any{
				uploaded,
				tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(source.URL + "/video")),
				tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(source.URL + "/photo")),
			}
			before := append([]any(nil), batch...)
			ids, err := sendPhotosWithIDs(bot, 42, 0, batch)
			if !reflect.DeepEqual(batch, before) {
				t.Error("input changed")
			}
			if unrelated {
				var apiErr *tgbotapi.Error
				if !errors.As(err, &apiErr) || apiErr.Code != 0 || calls != 1 || len(downloads) != 0 {
					t.Fatalf("error=%v calls=%d downloads=%v", err, calls, downloads)
				}
			} else if err != nil || calls != 3 || !reflect.DeepEqual(ids, []string{"p1", "v", "p2"}) || !reflect.DeepEqual(downloads, []string{"/video", "/photo"}) {
				t.Fatalf("ids=%v error=%v calls=%d downloads=%v", ids, err, calls, downloads)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Errorf("temporary files: %v %v", files, err)
			}
		})
	}
}
