package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type telegramTransport struct {
	target    string
	paths     []string
	transport http.RoundTripper
}

func (tr *telegramTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.paths = append(tr.paths, r.URL.String())
	r.URL.Scheme = "http"
	r.URL.Host = tr.target
	return tr.transport.RoundTrip(r)
}

func TestTelegramEndpointSelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
			endpoint := ""
			if local {
				endpoint = server.URL + "/"
			}
			t.Setenv("TELEGRAM_BOT_API_URL", endpoint)
			tr := &telegramTransport{target: strings.TrimPrefix(server.URL, "http://"), transport: http.DefaultTransport}
			old := http.DefaultTransport
			http.DefaultTransport = tr
			defer func() { http.DefaultTransport = old }()
			bot, err := newTelegramBot("token")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bot.MakeRequest("deleteWebhook", nil); err != nil {
				t.Fatal(err)
			}
			base := "https://api.telegram.org"
			if local {
				base = server.URL
			}
			for i, method := range []string{"getMe", "deleteWebhook"} {
				if tr.paths[i] != base+"/bottoken/"+method {
					t.Errorf("endpoint: %s", tr.paths[i])
				}
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestVideoRetryLargeAndLimit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		local, fail bool
	}{
		{"local", true, false}, {"local upload failure", true, true}, {"hosted chunked limit", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)

			if tc.local {
				t.Setenv("TELEGRAM_BOT_API_URL", "http://local")
			} else {
				t.Setenv("TELEGRAM_BOT_API_URL", "")
			}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.(http.Flusher).Flush()
				_, _ = io.CopyN(w, zeroReader{}, 51*1024*1024)
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
					_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"WEBPAGE_CURL_FAILED"}`)
					return
				}
				reader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				var size int64
				for {
					part, err := reader.NextPart()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Error(err)
						return
					}
					if part.FormName() == "video" {
						size, err = io.Copy(io.Discard, part)
						if err != nil {
							t.Error(err)
						}
					}
				}
				if size != 51*1024*1024 {
					t.Errorf("uploaded size %d", size)
				}
				if tc.fail {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"rejected"}`)
					return
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"video":{"file_id":"video"}}}`)
			}))
			defer telegram.Close()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}
			video := tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(source.URL + "/v.mp4"))
			_, err = sendPhotosWithIDs(bot, 42, 0, []any{video})
			if (err != nil) != (!tc.local || tc.fail) {
				t.Fatalf("error: %v", err)
			}
			if !tc.local && calls != 1 {
				t.Errorf("hosted limit: %d calls", calls)
			}
			files, err := filepath.Glob(filepath.Join(dir, "kopoka-video-*"))
			if err != nil || len(files) != 0 {
				t.Errorf("temporary files: %v %v", files, err)
			}
		})
	}
}

func TestInlineVideoURLRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "video")
	}))
	defer source.Close()
	calls := 0
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/deleteMessage") {
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
			return
		}
		calls++
		if err := r.ParseMultipartForm(1024 * 1024); err != nil {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
		}
		if r.FormValue("supports_streaming") != "true" || r.FormValue("caption") != "inline caption" || r.FormValue("duration") != "12" {
			t.Errorf("inline video parameters: %v", r.Form)
		}
		if calls == 1 {
			if r.FormValue("video") != source.URL {
				t.Errorf("video URL: %q", r.FormValue("video"))
			}
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"WEBPAGE_CURL_FAILED"}`)
			return
		}
		file, _, err := r.FormFile("video")
		if err != nil {
			t.Error(err)
			return
		}
		data, _ := io.ReadAll(file)
		_ = file.Close()
		if string(data) != "video" {
			t.Errorf("video: %q", data)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"video":{"file_id":"cached"}}}`)
	}))
	defer telegram.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	video := tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(source.URL))
	video.Caption = "inline caption"
	video.Duration = 12
	kind, id, err := cacheInlineMedia(bot, "42", video)
	if err != nil || kind != "video" || id != "cached" {
		t.Fatalf("%s %s %v", kind, id, err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Errorf("temporary files: %v", files)
	}
}

func TestOriginalVideoLargeUpload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	t.Setenv("TELEGRAM_BOT_API_URL", "http://local")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(w, zeroReader{}, 51*1024*1024)
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
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"failed to get HTTP URL content"}`)
			return
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		var size int64
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			if part.FormName() == "document" {
				size, err = io.Copy(io.Discard, part)
				if err != nil {
					t.Error(err)
				}
			}
		}
		if size != 51*1024*1024 {
			t.Errorf("size: %d", size)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer telegram.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	sources := []originalSource{{url: source.URL, kind: "video", index: 1}}
	sendOriginalBatch(bot, http.DefaultClient, 42, 0, sources, false, func(_ originalSource, err error) {
		t.Errorf("failure: %v", err)
	})
	if calls != 2 {
		t.Errorf("calls: %d", calls)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Errorf("temporary files: %v", files)
	}
}

func checkVideoUpload(t *testing.T, r *http.Request, name string) {
	t.Helper()
	file, _, err := r.FormFile(name)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "video bytes" {
		t.Errorf("upload: %q, %v", data, err)
	}
}

func checkStreamingAlbum(t *testing.T, r *http.Request, source string, calls int) {
	t.Helper()
	var media []map[string]any
	if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil || len(media) != 2 {
		t.Errorf("media: %q, %v", r.FormValue("media"), err)
		return
	}
	if media[0]["type"] != "video" || media[0]["supports_streaming"] != true || media[0]["caption"] != "caption" || media[0]["duration"] != float64(9) || media[0]["width"] != float64(640) {
		t.Errorf("video: %v", media[0])
	}
	if media[1]["type"] != "photo" || media[1]["supports_streaming"] != nil {
		t.Errorf("photo: %v", media[1])
	}
	if calls == 1 && media[0]["media"] != source {
		t.Errorf("URL: %v", media[0])
	}
	if calls == 2 {
		name, ok := media[0]["media"].(string)
		if !ok || !strings.HasPrefix(name, "attach://") {
			t.Errorf("attachment: %v", media[0])
			return
		}
		checkVideoUpload(t, r, strings.TrimPrefix(name, "attach://"))
	}
}

func streamingTelegramHandler(t *testing.T, album bool, source string, calls *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
			return
		}
		*calls++
		if err := r.ParseMultipartForm(1024 * 1024); err != nil {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
		}
		if album {
			checkStreamingAlbum(t, r, source, *calls)
		} else {
			if r.FormValue("supports_streaming") != "true" || r.FormValue("caption") != "caption" || r.FormValue("duration") != "9" {
				t.Errorf("parameters: %v", r.Form)
			}
			if *calls == 1 && r.FormValue("video") != source {
				t.Errorf("URL: %v", r.Form)
			}
			if *calls == 2 {
				checkVideoUpload(t, r, "video")
			}
		}
		if *calls == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"WEBPAGE_CURL_FAILED"}`)
			return
		}
		if album {
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"message_id":1},{"message_id":2}]}`)
		} else {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		}
	}
}

func TestArtworkVideoStreamingRequests(t *testing.T) {
	for _, album := range []bool{false, true} {
		t.Run(fmt.Sprintf("album=%t", album), func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "video bytes")
			}))
			defer source.Close()

			calls := 0
			telegram := httptest.NewServer(streamingTelegramHandler(t, album, source.URL, &calls))
			defer telegram.Close()
			bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
			if err != nil {
				t.Fatal(err)
			}

			video := tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(source.URL))
			video.Caption, video.Duration, video.Width = "caption", 9, 640
			media := []any{video}
			if album {
				media = append(media, tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL("https://example.com/photo.jpg")))
			}
			if _, err := sendPhotosWithIDs(bot, 42, 0, media); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Errorf("calls: %d", calls)
			}
		})
	}
}

func TestCachedVideoStreamingRequest(t *testing.T) {
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/sendVideo") || r.FormValue("video") != "cached-id" || r.FormValue("supports_streaming") != "true" {
			t.Errorf("request: %s %v", r.URL.Path, r.Form)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer telegram.Close()
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", telegram.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	video := tgbotapi.NewInputMediaVideo(tgbotapi.FileID("cached-id"))
	if err := sendPhotos(bot, 42, 0, []any{video}); err != nil {
		t.Fatal(err)
	}
}
