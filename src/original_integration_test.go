package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type originalDeliveryServer struct {
	t                             *testing.T
	server                        *httptest.Server
	documents, downloads, reports []string
}

func (s *originalDeliveryServer) sendDocument(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			s.t.Error(err)
			return
		}
		file, header, err := r.FormFile("document")
		if err != nil {
			s.t.Error(err)
			return
		}
		defer func() { _ = file.Close() }()
		data, _ := io.ReadAll(file)
		s.documents = append(s.documents, header.Filename+":"+string(data))
	} else {
		_ = r.ParseForm()
		raw := r.FormValue("document")
		s.documents = append(s.documents, raw)
		if strings.Contains(raw, "fallback") || strings.Contains(raw, "oversize") || strings.Contains(raw, "broken") {
			w.WriteHeader(400)
			_, _ = fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"URL rejected"}`)
			return
		}
	}
	if r.FormValue("chat_id") != "42" || r.FormValue("reply_to_message_id") != "7" {
		s.t.Errorf("document routing: %v", r.Form)
	}
	_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
}

func (s *originalDeliveryServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/bottoken/getMe":
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
	case "/bottoken/sendDocument":
		s.sendDocument(w, r)
	case "/bottoken/sendMessage":
		_ = r.ParseForm()
		if r.FormValue("parse_mode") != "HTML" || r.FormValue("reply_to_message_id") != "7" {
			s.t.Errorf("report routing: %v", r.Form)
		}

		s.reports = append(s.reports, r.FormValue("text"))
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":2}}`)
	case "/user/status/1":
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[
 {"type":"photo","url":"https://pbs.twimg.com/media/a.jpg?format=jpg&name=small"},
 {"type":"video","url":"%s/fallback.mp4"},
 {"type":"photo","url":"%s/oversize.jpg?a=1&b=2"},
 {"type":"gif","url":"%s/broken.gif"},{"type":"video","url":"%s/last.mp4"}]}}}`,
			s.server.URL, s.server.URL, s.server.URL, s.server.URL)
	case "/user/status/3":
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[
 {"type":"photo"},{"type":"video","url":"%s/last.mp4"}]}}}`, s.server.URL)
	case "/fallback.mp4", "/oversize.jpg", "/broken.gif", "/p0.jpg", "/p1.jpg", "/123_ugoira1920x1080.zip":
		s.downloads = append(s.downloads, r.URL.Path)
		if (strings.HasPrefix(r.URL.Path, "/p") || strings.HasSuffix(r.URL.Path, ".zip")) && r.Referer() != "https://www.pixiv.net/" {
			s.t.Errorf("referer = %q", r.Referer())
		}
		if r.URL.Path == "/broken.gif" {
			http.Error(w, "broken", 500)
			return
		}
		if r.URL.Path == "/oversize.jpg" {
			w.Header().Set("Content-Length", fmt.Sprint(originalFileLimit+1))
			return
		}
		_, _ = fmt.Fprint(w, "bytes")
	case "/auth/token":
		_, _ = fmt.Fprint(w, `{"response":{"access_token":"access","refresh_token":"refresh","expires_in":3600,"user":{}}}`)
	case "/v1/illust/detail":
		if r.URL.Query().Get("illust_id") == "2" {
			_, _ = fmt.Fprint(w, `{"illust":{"type":"ugoira"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"illust":{"type":"illust","meta_pages":[
 {"image_urls":{"original":"%s/p0.jpg","large":"%s/large.jpg"}},
 {"image_urls":{}},{"image_urls":{"original":"%s/p1.jpg"}}]}}`, s.server.URL, s.server.URL, s.server.URL)
	case "/v1/ugoira/metadata":
		_, _ = fmt.Fprintf(w, `{"ugoira_metadata":{"zip_urls":{"medium":"%s/123_ugoira600x600.zip"}}}`, s.server.URL)
	default:
		s.t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
	}
}

func TestOriginalDelivery(t *testing.T) {
	api := &originalDeliveryServer{t: t}
	api.server = httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	defer api.server.Close()

	target, _ := url.Parse(api.server.URL)
	old := http.DefaultTransport
	http.DefaultTransport = xPostTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.fxtwitter.com" || req.URL.Host == "api.vxtwitter.com" || req.URL.Host == "app-api.pixiv.net" || req.URL.Host == "oauth.secure.pixiv.net" {
			copy := req.Clone(req.Context())
			u := *req.URL
			u.Scheme, u.Host = target.Scheme, target.Host
			copy.URL = &u
			return old.RoundTrip(copy)
		}
		return old.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	bot, err := tgbotapi.NewBotAPIWithClient("token", api.server.URL+"/bot%s/%s", api.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	checkXOriginalDelivery(t, bot, api)
	checkPixivOriginalDelivery(t, bot, api)
	checkOriginalMissingURL(t, bot, api)
}

func checkXOriginalDelivery(t *testing.T, bot *tgbotapi.BotAPI, api *originalDeliveryServer) {
	t.Helper()
	x, _ := url.Parse("https://x.com/user/status/1")
	if err := sendOriginalFiles(bot, nil, 42, 7, x); err != nil {
		t.Fatal(err)
	}
	wantDocs := []string{
		"https://pbs.twimg.com/media/a.jpg?format=jpg&name=orig", api.server.URL + "/fallback.mp4",
		"fallback.mp4:bytes", api.server.URL + "/oversize.jpg?a=1&b=2",
		api.server.URL + "/broken.gif", api.server.URL + "/last.mp4",
	}
	if fmt.Sprint(api.documents) != fmt.Sprint(wantDocs) {
		t.Fatalf("documents = %v", api.documents)
	}
	wantReport := `failed files: unable to upload <a href="` + api.server.URL + `/oversize.jpg?a=1&amp;b=2">3rd file</a> for you` + "\n" +
		`failed files: unable to upload <a href="` + api.server.URL + `/broken.gif">4th file</a> for you`
	if len(api.reports) != 1 || api.reports[0] != wantReport {
		t.Fatalf("reports = %v", api.reports)
	}
	if fmt.Sprint(api.downloads) != "[/fallback.mp4 /oversize.jpg /broken.gif]" {
		t.Fatalf("downloads = %v", api.downloads)
	}
}

func checkPixivOriginalDelivery(t *testing.T, bot *tgbotapi.BotAPI, api *originalDeliveryServer) {
	t.Helper()
	api.documents, api.downloads, api.reports = nil, nil, nil
	_, err := pixiv.LoadAuth("access", "refresh", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	app := pixiv.NewApp()
	for _, raw := range []string{"https://www.pixiv.net/artworks/1", "https://www.pixiv.net/artworks/2"} {
		link, _ := url.Parse(raw)
		if err := sendOriginalFiles(bot, app, 42, 7, link); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(api.documents) != "[p0.jpg:bytes p1.jpg:bytes 123_ugoira1920x1080.zip:bytes]" {
		t.Fatalf("Pixiv documents = %v", api.documents)
	}
	if fmt.Sprint(api.downloads) != "[/p0.jpg /p1.jpg /123_ugoira1920x1080.zip]" {
		t.Fatalf("Pixiv downloads = %v", api.downloads)
	}
	if len(api.reports) != 1 || api.reports[0] != "failed files: unable to upload 2nd file for you" {
		t.Fatalf("Pixiv reports = %v", api.reports)
	}
}

func checkOriginalMissingURL(t *testing.T, bot *tgbotapi.BotAPI, api *originalDeliveryServer) {
	t.Helper()
	api.documents, api.reports = nil, nil
	x, _ := url.Parse("https://x.com/user/status/3")
	if err := sendOriginalFiles(bot, nil, 42, 7, x); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(api.documents) != "["+api.server.URL+"/last.mp4]" || len(api.reports) != 1 || api.reports[0] != "failed files: unable to upload 1st file for you" {
		t.Fatalf("X missing URL: documents = %v, reports = %v", api.documents, api.reports)
	}
}

func TestOriginalChunkedOversize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		chunk := make([]byte, 64*1024)
		for sent := 0; sent <= originalFileLimit; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	err := sendOriginalFile(nil, server.Client(), 1, 1, server.URL+"/original.png", true)
	if err == nil {
		t.Fatalf("oversized chunked download: %v", err)
	}
}
