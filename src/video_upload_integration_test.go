package main

import (
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
		if calls == 1 {
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
	kind, id, err := cacheInlineMedia(bot, "42", tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(source.URL)))
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
